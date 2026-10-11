package main

import (
	"embed"
	"fmt"
	"image"
	"image/color"
	_ "image/png" // 注册PNG解码器（副作用导入）
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"fyne.io/systray"
	"go.bug.st/serial"
	"golang.org/x/sys/windows"
)

type Config struct {
	ComPort       string
	BaudRate      int
	WindowTitle   string
	CheckInterval int
	IdleTimeout   int // 空闲超时（秒），超过此时间无键盘鼠标输入则暂停识别，0=禁用
}

var cfg = Config{
	ComPort:       "COM9",
	BaudRate:      2400,
	WindowTitle:   "云零售",
	CheckInterval: 800,
	IdleTimeout:   30,
}

//go:embed app.ico
var iconBytes []byte

//go:embed templates/*.png
var templateFS embed.FS

//go:embed templates_half/*.png
var templateHalfFS embed.FS

// version 编译时通过 -ldflags "-X main.version=1.0.0" 注入
var version = "dev"

var (
	ser                 serial.Port
	lastAmount          = ""
	lastDisplayedAmount = ""
	exeDir              string
	exiting             bool
	amountRe            = regexp.MustCompile(`\d+\.?\d*`)
	lastW, lastH        = 0, 0 // 上次窗口尺寸，用于检测全屏/窗口化切换
	switching           = false // 窗口切换中，跳过OCR保持上次结果
	stableCount         = 0     // 尺寸连续不变的周期数

	mStatus  *systray.MenuItem
	mSerial  *systray.MenuItem
	mOverlay *systray.MenuItem
	mDebug   *systray.MenuItem
	serialOK bool
	overlayVisible = true // 悬浮窗显示状态
	debugMode      = false // 调试模式开关

	// 缓存窗口句柄
	cachedHwnd  windows.HWND
	lastOCR     string

	// 金额悬浮窗（实时显示识别金额）
	debugHwnd    windows.HWND
	debugStatic  windows.HWND
	debugFont    uintptr
	overlayCtlBrush uintptr // WM_CTLCOLORSTATIC 复用的背景刷，避免GDI泄漏
	overlayW     = 240
	overlayH     = 70
	overlayX     = -1 // -1表示未保存，用默认位置
	overlayY     = -1

	// 模板匹配OCR
	tmplLoaded  bool
	tmplMap     map[string]*image.Gray
	tmplMapHalf map[string]*image.Gray
	tmplStdW    = 60
	tmplStdH    = 60
)

const (
	hkeyCurrentUser = 0x80000001
	keyRead         = 0x20019
	keyWrite        = 0x20006
	regSz           = 1
	regDword        = 4
)

func regGetString(hKey uintptr, name, def string) string {
	namePtr, _ := syscall.UTF16PtrFromString(name)
	var bufSize uint32 = 512
	buf := make([]uint16, bufSize)
	ret, _, _ := procRegQueryValueExW.Call(hKey, uintptr(unsafe.Pointer(namePtr)), 0, 0,
		uintptr(unsafe.Pointer(&buf[0])), uintptr(unsafe.Pointer(&bufSize)))
	if ret != 0 {
		return def
	}
	return windows.UTF16ToString(buf[:bufSize/2])
}

func regGetDWord(hKey uintptr, name string, def int) int {
	namePtr, _ := syscall.UTF16PtrFromString(name)
	var val uint32
	var bufSize uint32 = 4
	ret, _, _ := procRegQueryValueExW.Call(hKey, uintptr(unsafe.Pointer(namePtr)), 0, 0,
		uintptr(unsafe.Pointer(&val)), uintptr(unsafe.Pointer(&bufSize)))
	if ret != 0 {
		return def
	}
	return int(val)
}

func regSetString(hKey uintptr, name, val string) {
	namePtr, _ := syscall.UTF16PtrFromString(name)
	valPtr, _ := syscall.UTF16PtrFromString(val)
	procRegSetValueExW.Call(hKey, uintptr(unsafe.Pointer(namePtr)), 0, regSz,
		uintptr(unsafe.Pointer(valPtr)), uintptr((len(val)+1)*2))
}

func regSetDWord(hKey uintptr, name string, val uint32) {
	namePtr, _ := syscall.UTF16PtrFromString(name)
	procRegSetValueExW.Call(hKey, uintptr(unsafe.Pointer(namePtr)), 0, regDword,
		uintptr(unsafe.Pointer(&val)), 4)
}

func loadConfig() {
	path, _ := syscall.UTF16PtrFromString(`Software\客屏对接`)
	var hKey uintptr
	ret, _, _ := procRegOpenKeyExW.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(path)), 0, keyRead, uintptr(unsafe.Pointer(&hKey)))
	if ret != 0 {
		return // 第一次运行，用默认值
	}
	defer procRegCloseKey.Call(hKey)
	cfg.ComPort = regGetString(hKey, "com_port", cfg.ComPort)
	cfg.WindowTitle = regGetString(hKey, "window_title", cfg.WindowTitle)
	cfg.BaudRate = regGetDWord(hKey, "baud_rate", cfg.BaudRate)
	cfg.CheckInterval = regGetDWord(hKey, "check_interval_ms", cfg.CheckInterval)
	cfg.IdleTimeout = regGetDWord(hKey, "idle_timeout", cfg.IdleTimeout)
}

func saveConfig() {
	path, _ := syscall.UTF16PtrFromString(`Software\客屏对接`)
	var hKey, disp uintptr
	procRegCreateKeyExW.Call(hkeyCurrentUser, uintptr(unsafe.Pointer(path)), 0, 0, 0, keyWrite, 0,
		uintptr(unsafe.Pointer(&hKey)), uintptr(unsafe.Pointer(&disp)))
	defer procRegCloseKey.Call(hKey)
	regSetString(hKey, "com_port", cfg.ComPort)
	regSetString(hKey, "window_title", cfg.WindowTitle)
	regSetDWord(hKey, "baud_rate", uint32(cfg.BaudRate))
	regSetDWord(hKey, "check_interval_ms", uint32(cfg.CheckInterval))
	regSetDWord(hKey, "idle_timeout", uint32(cfg.IdleTimeout))
}

func main() {
	exe, _ := os.Executable()
	exeDir = filepath.Dir(exe)
	loadConfig()

	// 日志输出到文件，用Dup2重定向原始stderr文件描述符，捕获Go runtime fatal error
	logPath := filepath.Join(configDir(), "app.log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err == nil {
		log.SetOutput(logFile)
		// 重定向stderr文件描述符，捕获Go runtime fatal error
		procSetStdHandle.Call(^uintptr(11), logFile.Fd()) // STD_ERROR_HANDLE = -12
	}
	log.Printf("=== 客屏对接 %s 启动 ===", version)

	openSerial()
	for {
		exiting = false
		systray.Run(onReady, onExit)
		log.Printf("systray.Run 返回，exiting=%v", exiting)
		if exiting {
			break // 用户主动退出
		}
		// systray意外退出（如Explorer重启），等待3秒后重新初始化
		log.Printf("systray意外退出，3秒后重启")
		time.Sleep(3 * time.Second)
	}
}

var initialized = false

func onReady() {
	systray.SetIcon(iconBytes)
	systray.SetTitle("客屏对接 " + version)
	systray.SetTooltip("客屏对接 " + version)

	mStatus = systray.AddMenuItem("状态: 启动中", "当前运行状态")
	mStatus.Disable()
	mSerial = systray.AddMenuItem("串口: -", "串口连接状态")
	mSerial.Disable()

	systray.AddSeparator()
	mOverlay = systray.AddMenuItemCheckbox("显示悬浮窗", "显示/隐藏金额悬浮窗", true)
	mDebug = systray.AddMenuItemCheckbox("调试模式", "开启详细OCR识别日志", false)
	mLog := systray.AddMenuItem("打开日志", "打开日志文件查看识别详情")
	systray.AddSeparator()
	mReinit := systray.AddMenuItem("重新初始化", "重新加载模板、重连串口、清空缓存")
	mSettings := systray.AddMenuItem("设置", "修改串口和检查间隔")
	systray.AddSeparator()
	mAbout := systray.AddMenuItem("关于", "版本信息")
	mQuit := systray.AddMenuItem("退出", "退出客屏对接程序")

	safeGo("overlay-menu", func() {
		for range mOverlay.ClickedCh {
			toggleOverlay()
		}
	})
	safeGo("debug-menu", func() {
		for range mDebug.ClickedCh {
			debugMode = !debugMode
			if debugMode {
				mDebug.Check()
				showMsg("调试模式已开启\n详细识别信息将写入日志")
			} else {
				mDebug.Uncheck()
			}
		}
	})
	safeGo("log-menu", func() {
		for range mLog.ClickedCh {
			logPath := filepath.Join(configDir(), "app.log")
			exec.Command("rundll32", "url.dll,FileProtocolHandler", logPath).Start()
		}
	})
	safeGo("reinit-menu", func() {
		for range mReinit.ClickedCh {
			reinitialize()
		}
	})
	safeGo("settings-menu", func() {
		for range mSettings.ClickedCh {
			safeGo("settings", showSettings)
		}
	})
	safeGo("about-menu", func() {
		for range mAbout.ClickedCh {
			showMsg("客屏对接 " + version + "\n\nPrintWindow截屏 + 数字模板匹配\n纯内存OCR，串口输出客显金额")
		}
	})
	safeGo("quit-menu", func() {
		<-mQuit.ClickedCh
		cleanupAndExit()
		systray.Quit()
	})

	updateSerialMenu()
	if !initialized {
		initialized = true
		safeGo("overlay", createDebugOverlay)
		loadTemplates()
		safeGo("runloop", runLoop)
	}
}

func cleanupAndExit() {
	log.Printf("cleanupAndExit 被调用，开始清理")
	exiting = true
	time.Sleep(time.Second) // 等 runLoop 停手
	if ser != nil {
		ser.Write([]byte{0x1B, 0x40}) // 初始化
		time.Sleep(100 * time.Millisecond)
		ser.Write([]byte{0x0C}) // 清屏
		time.Sleep(200 * time.Millisecond)
		ser.Close()
	}
	if debugHwnd != 0 {
		procDestroyWindow.Call(uintptr(debugHwnd))
	}
	log.Printf("cleanupAndExit 完成")
}

func onExit() {
	log.Printf("onExit 被调用（systray已退出），exiting=%v", exiting)
	// 不在这里清理：如果是用户退出，cleanupAndExit已在菜单处理中调用
	// 如果是意外退出，主循环会重启systray，不需要清理
}

func updateStatus(line string) {
	if mStatus != nil {
		mStatus.SetTitle(line)
	}
	systray.SetTooltip(line)
}

func updateSerialMenu() {
	if mSerial == nil {
		return
	}
	if serialOK {
		mSerial.SetTitle(fmt.Sprintf("串口: %s @ %d (已连接)", cfg.ComPort, cfg.BaudRate))
	} else {
		mSerial.SetTitle(fmt.Sprintf("串口: %s (未连接)", cfg.ComPort))
	}
}

// getWindow 优先用缓存的窗口句柄，失效时才重新枚举
func getWindow(title string) windows.HWND {
	if cachedHwnd != 0 {
		// 只检查窗口是否还存在（不检查可见性，最小化窗口也有效）
		ret, _, _ := procIsWindow.Call(uintptr(cachedHwnd))
		if ret != 0 {
			return cachedHwnd
		}
		cachedHwnd = 0
	}
	cachedHwnd = findWindow(title)
	return cachedHwnd
}

// safeGo 启动带 recover 保护的 goroutine，避免 panic 导致整个程序退出
func safeGo(name string, f func()) {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[%s] panic: %v", name, r)
			}
		}()
		f()
	}()
}

// getIdleSeconds 返回距离上次键盘/鼠标/扫描枪输入的秒数
func getIdleSeconds() int {
	var li lastInputInfo
	li.CbSize = uint32(unsafe.Sizeof(li))
	ret, _, _ := procGetLastInputInfo.Call(uintptr(unsafe.Pointer(&li)))
	if ret == 0 {
		return 0
	}
	// GetTickCount 返回低32位有效，需截断避免64位垃圾值
	tick, _, _ := procGetTickCount.Call()
	return int((uint32(tick) - li.DwTime) / 1000)
}

func runLoop() {
	for {
		func() {
			defer func() {
				if r := recover(); r != nil {
					log.Printf("错误: %v", r)
					updateStatus(fmt.Sprintf("错误: %v", r))
				}
			}()
			hwnd := getWindow(cfg.WindowTitle)
			if hwnd == 0 {
				updateStatus("状态: 未找到收银窗口")
				time.Sleep(2 * time.Second)
				return
			}
			// 最小化窗口 PrintWindow 捕获不到内容（Flutter不响应WM_PRINT），
			// 最小化时收银金额不会变化，跳过识别
			if ret, _, _ := procIsIconic.Call(uintptr(hwnd)); ret != 0 {
				updateDebugOverlay("待机")
				lastDisplayedAmount = ""
				return
			}
			// 空闲超时：超过设定时间无键盘/鼠标/扫描枪输入则暂停识别，节省资源
			if cfg.IdleTimeout > 0 && getIdleSeconds() >= cfg.IdleTimeout {
				updateDebugOverlay("待机")
				lastDisplayedAmount = "" // 清空，恢复后第一次识别结果会更新显示
				return
			}
			img := captureWindow(hwnd) // PrintWindow 截收银主窗口客户区
			if img != nil {
				amount := ocrAmount(img)
				if amount != "" {
					if amount != lastDisplayedAmount {
						updateStatus("状态: 客显显示 " + amount)
						updateDebugOverlay(amount)
						lastDisplayedAmount = amount
					}
					sendAmount(amount)
				} else if lastDisplayedAmount == "" {
					// 从未识别到金额时才显示"识别中"
					updateStatus("状态: 识别金额中...")
					updateDebugOverlay("识别中...")
				}
				// amount=="" 但已有 lastDisplayedAmount 时，保持显示上次金额（防抖，避免光标闪烁导致跳动）
			}
		}()
		time.Sleep(time.Duration(cfg.CheckInterval) * time.Millisecond)
	}
}

func reinitialize() {
	cachedHwnd = 0
	lastOCR = ""
	lastAmount = ""
	lastDisplayedAmount = ""
	loadTemplates()
	if ser != nil {
		ser.Close()
		ser = nil
	}
	openSerial()
	showMsg("已重新初始化，将重新识别收银窗口")
}

func openSerial() {
	mode := &serial.Mode{BaudRate: cfg.BaudRate}
	var err error
	ser, err = serial.Open(cfg.ComPort, mode)
	if err != nil {
		log.Printf("串口打开失败: %v", err)
		serialOK = false
		updateSerialMenu()
		return
	}
	ser.Write([]byte{0x1B, 0x40})
	time.Sleep(100 * time.Millisecond)
	ser.Write([]byte{0x0C})
	time.Sleep(100 * time.Millisecond)
	serialOK = true
	updateSerialMenu()
}

func sendAmount(amount string) {
	if exiting {
		return
	}
	if amount == lastAmount {
		return
	}
	lastAmount = amount
	if ser == nil {
		openSerial()
		if ser == nil {
			return
		}
	}
	ser.Write([]byte{0x0C})
	time.Sleep(20 * time.Millisecond)
	data := append([]byte{0x1B, 0x51, 0x41}, []byte(amount)...)
	data = append(data, 0x0D)
	ser.Write(data)
}

var (
	user32   = windows.NewLazySystemDLL("user32.dll")
	gdi32    = windows.NewLazySystemDLL("gdi32.dll")
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	advapi32 = windows.NewLazySystemDLL("advapi32.dll")
	procEnumWindows            = user32.NewProc("EnumWindows")
	procGetWindowTextW         = user32.NewProc("GetWindowTextW")
	procGetClassNameW          = user32.NewProc("GetClassNameW")
	procIsWindowVisible        = user32.NewProc("IsWindowVisible")
	procIsWindow               = user32.NewProc("IsWindow")
	procGetWindowRect          = user32.NewProc("GetWindowRect")
	procGetClientRect          = user32.NewProc("GetClientRect")
	procClientToScreen         = user32.NewProc("ClientToScreen")
	procGetWindowDC            = user32.NewProc("GetWindowDC")
	procReleaseDC              = user32.NewProc("ReleaseDC")
	procPrintWindow            = user32.NewProc("PrintWindow")
	procGetWindowPlacement     = user32.NewProc("GetWindowPlacement")
	procIsIconic               = user32.NewProc("IsIconic")
	procGetLastInputInfo       = user32.NewProc("GetLastInputInfo")
	procSetWindowPlacement     = user32.NewProc("SetWindowPlacement")
	procCreateCompatibleDC     = gdi32.NewProc("CreateCompatibleDC")
	procCreateDIBSection       = gdi32.NewProc("CreateDIBSection")
	procSelectObject           = gdi32.NewProc("SelectObject")
	procDeleteObject           = gdi32.NewProc("DeleteObject")
	procDeleteDC               = gdi32.NewProc("DeleteDC")
	procGetStockObject         = gdi32.NewProc("GetStockObject")
	procCreateFontW            = gdi32.NewProc("CreateFontW")
	procSetTextColor           = gdi32.NewProc("SetTextColor")
	procSetBkColor             = gdi32.NewProc("SetBkColor")
	procCreateSolidBrush       = gdi32.NewProc("CreateSolidBrush")
	procCreateRoundRectRgn     = gdi32.NewProc("CreateRoundRectRgn")
	procCreatePopupMenu        = user32.NewProc("CreatePopupMenu")
	procAppendMenuW            = user32.NewProc("AppendMenuW")
	procTrackPopupMenu         = user32.NewProc("TrackPopupMenu")
	procDestroyMenu            = user32.NewProc("DestroyMenu")
	procSetWindowRgn           = user32.NewProc("SetWindowRgn")
	procSetWindowPos           = user32.NewProc("SetWindowPos")
	procInvalidateRect         = user32.NewProc("InvalidateRect")

	// 设置对话框用的 API
	procRegisterClassExW   = user32.NewProc("RegisterClassExW")
	procCreateWindowExW    = user32.NewProc("CreateWindowExW")
	procMessageBoxW         = user32.NewProc("MessageBoxW")
	procGetMessageW        = user32.NewProc("GetMessageW")
	procTranslateMessage   = user32.NewProc("TranslateMessage")
	procDispatchMessageW   = user32.NewProc("DispatchMessageW")
	procDefWindowProcW     = user32.NewProc("DefWindowProcW")
	procPostQuitMessage    = user32.NewProc("PostQuitMessage")
	procLoadCursorW        = user32.NewProc("LoadCursorW")
	procGetWindowTextLengthW = user32.NewProc("GetWindowTextLengthW")
	procDestroyWindow      = user32.NewProc("DestroyWindow")
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
	procSendMessageW        = user32.NewProc("SendMessageW")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	procShowWindow         = user32.NewProc("ShowWindow")
	procUpdateWindow       = user32.NewProc("UpdateWindow")
	procSetWindowTextW     = user32.NewProc("SetWindowTextW")
	procGetModuleHandle    = kernel32.NewProc("GetModuleHandleW")
	procGetTickCount       = kernel32.NewProc("GetTickCount")
	procSetStdHandle       = kernel32.NewProc("SetStdHandle")

	// 注册表 API
	procRegOpenKeyExW    = advapi32.NewProc("RegOpenKeyExW")
	procRegQueryValueExW = advapi32.NewProc("RegQueryValueExW")
	procRegSetValueExW   = advapi32.NewProc("RegSetValueExW")
	procRegCreateKeyExW  = advapi32.NewProc("RegCreateKeyExW")
	procRegCloseKey      = advapi32.NewProc("RegCloseKey")
)

type rect struct{ Left, Top, Right, Bottom int32 }
type point struct{ X, Y int32 }
type windowPlacement struct {
	Length            uint32
	Flags             uint32
	ShowCmd           uint32
	PtMinPosition     point
	PtMaxPosition     point
	RcNormalPosition  rect
}
type lastInputInfo struct {
	CbSize uint32
	DwTime uint32
}
type bitmapInfoHeader struct {
	Size                   uint32
	Width, Height          int32
	Planes, BitCount       uint16
	Compression, SizeImage uint32
	XPels, YPels           uint32
	ClrUsed, ClrImportant  uint32
}

type wndClassEx struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type winMsg struct {
	Hwnd    windows.HWND
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      point
}

// 设置对话框控件句柄
var (
	dlgHwnd     windows.HWND
	dlgCom      windows.HWND
	dlgBaud     windows.HWND
	dlgInterval windows.HWND
	dlgIdle     windows.HWND
)

const (
	ID_COM      = 1001
	ID_BAUD     = 1002
	ID_INTERVAL = 1003
	ID_SAVE     = 1004
	ID_CLEAR    = 1005
	ID_REINIT   = 1006
	ID_IDLE     = 1007
)

// settingsDlgProc 设置对话框窗口过程
func settingsDlgProc(hwnd windows.HWND, msg uint32, wParam, lParam uintptr) (ret uintptr) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[settingsDlgProc] panic: %v", r)
			ret = 0
		}
	}()
	switch msg {
	case 0x0138: // WM_CTLCOLORSTATIC — 静态文本背景和对话框一致
		procSetBkColor.Call(wParam, 0xF0F0F0) // COLOR_3DFACE
		brush, _, _ := procGetStockObject.Call(15)
		return brush
	case 0x0111: // WM_COMMAND
		if wParam == ID_SAVE {
			// 读取编辑框内容
			cfg.ComPort = getEditText(dlgCom)
			if v, err := strconv.Atoi(getEditText(dlgBaud)); err == nil {
				cfg.BaudRate = v
			}
			if v, err := strconv.Atoi(getEditText(dlgInterval)); err == nil {
				if v < 100 {
					v = 100
				}
				if v > 5000 {
					v = 5000
				}
				cfg.CheckInterval = v
				setWindowText(dlgInterval, strconv.Itoa(v))
			}
			if v, err := strconv.Atoi(getEditText(dlgIdle)); err == nil {
				if v < 0 {
					v = 0
				}
				if v > 3600 {
					v = 3600
				}
				cfg.IdleTimeout = v
				setWindowText(dlgIdle, strconv.Itoa(v))
			}
			saveConfig()
			// 重连串口
			if ser != nil {
				ser.Close()
				ser = nil
			}
			lastAmount = ""
			lastDisplayedAmount = ""
			openSerial()
			showMsg("已保存，串口已重连")
		} else if wParam == ID_CLEAR {
			// 清屏客显
			if ser != nil {
				ser.Write([]byte{0x1B, 0x40})
				time.Sleep(50 * time.Millisecond)
				ser.Write([]byte{0x0C})
				showMsg("客显已清屏")
			} else {
				showMsg("串口未打开，无法清屏")
			}
		} else if wParam == ID_REINIT {
			reinitialize()
		}
	case 0x0010: // WM_CLOSE
		procDestroyWindow.Call(uintptr(hwnd))
	case 0x0002: // WM_DESTROY
		dlgHwnd = 0
		procPostQuitMessage.Call(0)
	default:
		ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
		return ret
	}
	return 0
}

func getEditText(hwnd windows.HWND) string {
	n, _, _ := procGetWindowTextLengthW.Call(uintptr(hwnd))
	if n == 0 {
		return ""
	}
	buf := make([]uint16, n+1)
	procGetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), uintptr(n+1))
	return windows.UTF16ToString(buf)
}

func setWindowText(hwnd windows.HWND, text string) {
	p, _ := syscall.UTF16PtrFromString(text)
	procSetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(p)))
}

func showMsg(text string) {
	msg, _ := syscall.UTF16PtrFromString(text)
	title, _ := syscall.UTF16PtrFromString("提示")
	procMessageBoxW.Call(0, uintptr(unsafe.Pointer(msg)), uintptr(unsafe.Pointer(title)), 0x40)
}

func createControl(className, text string, x, y, w, h int, parent windows.HWND, id int) windows.HWND {
	cn, _ := syscall.UTF16PtrFromString(className)
	tx, _ := syscall.UTF16PtrFromString(text)
	hwnd, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(cn)),
		uintptr(unsafe.Pointer(tx)),
		0x40000000|0x10000000, // WS_CHILD|WS_VISIBLE
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		uintptr(parent), uintptr(id), 0, 0,
	)
	// 设置系统默认GUI字体
	font, _, _ := procGetStockObject.Call(17) // DEFAULT_GUI_FONT
	procSendMessageW.Call(hwnd, 0x0030, font, 1) // WM_SETFONT
	return windows.HWND(hwnd)
}

// showSettings 弹出设置对话框（Windows API 原生，无额外依赖）
func showSettings() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if dlgHwnd != 0 {
		// 已打开，置前
		procSetForegroundWindow.Call(uintptr(dlgHwnd))
		return
	}

	className, _ := syscall.UTF16PtrFromString("客屏对接设置类")
	instance, _, _ := procGetModuleHandle.Call(0)

	var wc wndClassEx
	wc.Size = uint32(unsafe.Sizeof(wc))
	wc.Style = 0x0003 // CS_HREDRAW|CS_VREDRAW
	wc.WndProc = syscall.NewCallback(settingsDlgProc)
	wc.Instance = instance
	wc.Cursor, _, _ = procLoadCursorW.Call(0, 32512) // IDC_ARROW
	wc.Background, _, _ = procGetStockObject.Call(15) // COLOR_3DFACE 系统对话框背景
	wc.ClassName = className
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	title, _ := syscall.UTF16PtrFromString("客屏对接设置")
	// 窗口居中
	sw, _, _ := procGetSystemMetrics.Call(0)
	sh, _, _ := procGetSystemMetrics.Call(1)
	dlgW, dlgH := 420, 255
	dlgX := (int(sw) - dlgW) / 2
	dlgY := (int(sh) - dlgH) / 2
	hwndPtr, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		0x00CF0000, // WS_OVERLAPPEDWINDOW
		uintptr(dlgX), uintptr(dlgY), uintptr(dlgW), uintptr(dlgH),
		0, 0, instance, 0,
	)
	dlgHwnd = windows.HWND(hwndPtr)

	// 串口设置
	createControl("STATIC", "COM 端口:", 20, 24, 90, 24, dlgHwnd, 0)
	dlgCom = createControl("EDIT", cfg.ComPort, 120, 20, 270, 30, dlgHwnd, ID_COM)
	createControl("STATIC", "波特率:", 20, 64, 90, 24, dlgHwnd, 0)
	dlgBaud = createControl("EDIT", strconv.Itoa(cfg.BaudRate), 120, 60, 270, 30, dlgHwnd, ID_BAUD)
	createControl("STATIC", "检查间隔（毫秒）:", 20, 104, 140, 24, dlgHwnd, 0)
	dlgInterval = createControl("EDIT", strconv.Itoa(cfg.CheckInterval), 165, 100, 225, 30, dlgHwnd, ID_INTERVAL)
	createControl("STATIC", "空闲暂停（秒，0=禁用）:", 20, 144, 160, 24, dlgHwnd, 0)
	dlgIdle = createControl("EDIT", strconv.Itoa(cfg.IdleTimeout), 190, 140, 200, 30, dlgHwnd, ID_IDLE)

	// 操作按钮
	createControl("BUTTON", "保存", 55, 190, 95, 36, dlgHwnd, ID_SAVE)
	createControl("BUTTON", "清屏客显", 165, 190, 95, 36, dlgHwnd, ID_CLEAR)
	createControl("BUTTON", "重新初始化", 275, 190, 105, 36, dlgHwnd, ID_REINIT)

	procShowWindow.Call(uintptr(dlgHwnd), 5) // SW_SHOW
	procUpdateWindow.Call(uintptr(dlgHwnd))

	// 消息循环
	var m winMsg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if ret == 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// === 金额悬浮窗（实时显示识别金额）===

const (
	WM_SETFONT      = 0x0030
	ID_OVERLAY_CLOSE = 3001
	ID_OVERLAY_SETTINGS = 3002
	ID_OVERLAY_RESET = 3003
)

// overlayBgColor 背景色（深蓝灰）
var overlayBgColor = COLORREF(0x2B2B3C)
var overlayTextColor = COLORREF(0x00E5FF) // 青色文字
var overlayTitleColor = COLORREF(0x8888AA) // 灰紫色标题

type COLORREF uint32

func overlayWndProc(hwnd windows.HWND, msg uint32, wParam, lParam uintptr) (ret uintptr) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[overlayWndProc] panic: %v", r)
			ret = 0
		}
	}()
	switch msg {
	case 0x0138: // WM_CTLCOLORSTATIC
		procSetTextColor.Call(wParam, uintptr(overlayTextColor))
		procSetBkColor.Call(wParam, uintptr(overlayBgColor))
		if overlayCtlBrush == 0 {
			overlayCtlBrush, _, _ = procCreateSolidBrush.Call(uintptr(overlayBgColor))
		}
		return overlayCtlBrush
	case 0x0111: // WM_COMMAND
		if wParam == ID_OVERLAY_CLOSE {
			hideOverlay()
		} else if wParam == ID_OVERLAY_SETTINGS {
			safeGo("settings", showSettings)
		} else if wParam == ID_OVERLAY_RESET {
			resetOverlaySize()
		}
	case 0x00A5: // WM_NCRBUTTONUP — HTCAPTION模式下右键触发非客户区消息
		x := int(int16(lParam & 0xFFFF))
		y := int(int16((lParam >> 16) & 0xFFFF))
		showOverlayContextMenu(hwnd, x, y, true)
	case 0x0005: // WM_SIZE — 窗口大小变化，自适应字体+更新圆角
		newW := int(uint16(lParam & 0xFFFF))
		newH := int(uint16((lParam >> 16) & 0xFFFF))
		if newW > 0 && newH > 0 {
			overlayW, overlayH = newW, newH
			applyOverlayFont()
			// 更新圆角区域
			hrgn, _, _ := procCreateRoundRectRgn.Call(0, 0, uintptr(newW+1), uintptr(newH+1), 16, 16)
			procSetWindowRgn.Call(uintptr(hwnd), hrgn, 1)
			saveOverlaySize()
		}
	case 0x0003: // WM_MOVE — 窗口位置变化，保存位置
		overlayX = int(int16(lParam & 0xFFFF))
		overlayY = int(int16((lParam >> 16) & 0xFFFF))
		saveOverlaySize()
	case 0x0002: // WM_DESTROY
		debugHwnd = 0
		if debugFont != 0 {
			procDeleteObject.Call(debugFont)
			debugFont = 0
		}
		if overlayCtlBrush != 0 {
			procDeleteObject.Call(overlayCtlBrush)
			overlayCtlBrush = 0
		}
	case 0x0084: // WM_NCHITTEST — 边框可调整大小，中间可拖动
		return overlayHitTest(hwnd, lParam)
	default:
		ret, _, _ := procDefWindowProcW.Call(uintptr(hwnd), uintptr(msg), wParam, lParam)
		return ret
	}
	return 0
}

// overlayHitTest 检测鼠标位置，返回对应命中测试值（边框调整或拖动）
func overlayHitTest(hwnd windows.HWND, lParam uintptr) uintptr {
	border := 8 // 边框检测宽度
	ptX := int(int16(lParam & 0xFFFF))
	ptY := int(int16((lParam >> 16) & 0xFFFF))
	var rc rect
	procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&rc)))
	w := int(rc.Right - rc.Left)
	h := int(rc.Bottom - rc.Top)
	// 转换为客户区坐标
	x := ptX - int(rc.Left)
	y := ptY - int(rc.Top)
	// 检测四个角
	if x < border && y < border {
		return 13 // HTTOPLEFT
	}
	if x >= w-border && y < border {
		return 14 // HTTOPRIGHT
	}
	if x < border && y >= h-border {
		return 16 // HTBOTTOMLEFT
	}
	if x >= w-border && y >= h-border {
		return 17 // HTBOTTOMRIGHT
	}
	// 检测四条边
	if y < border {
		return 12 // HTTOP
	}
	if y >= h-border {
		return 15 // HTBOTTOM
	}
	if x < border {
		return 10 // HTLEFT
	}
	if x >= w-border {
		return 11 // HTRIGHT
	}
	// 中间区域可拖动
	return 2 // HTCAPTION
}

// applyOverlayFont 根据当前窗口高度创建合适大小的字体并应用
func applyOverlayFont() {
	if debugHwnd == 0 || debugStatic == 0 {
		return
	}
	fontSize := int(float64(overlayH) * 0.50)
	if fontSize < 16 {
		fontSize = 16
	}
	if fontSize > 120 {
		fontSize = 120
	}
	// 删除旧字体
	if debugFont != 0 {
		procDeleteObject.Call(debugFont)
		debugFont = 0
	}
	fontPtr, _, _ := procCreateFontW.Call(
		uintptr(fontSize), 0, 0, 0, 700,
		0, 0, 0, 0, 0, 0, 0, 0,
		uintptr(unsafe.Pointer(syscall.StringToUTF16Ptr("Consolas"))),
	)
	debugFont = fontPtr
	procSendMessageW.Call(uintptr(debugStatic), WM_SETFONT, debugFont, 1)
	// 调整静态文本大小，垂直居中
	textH := int(float64(overlayH) * 0.80)
	textY := (overlayH - textH) / 2
	procSetWindowPos.Call(uintptr(debugStatic), 0, 0, uintptr(textY), uintptr(overlayW), uintptr(textH), 0x0014)
	// 更新圆角
	hrgn, _, _ := procCreateRoundRectRgn.Call(0, 0, uintptr(overlayW+1), uintptr(overlayH+1), 16, 16)
	procSetWindowRgn.Call(uintptr(debugHwnd), hrgn, 1)
	procInvalidateRect.Call(uintptr(debugHwnd), 0, 1)
}

// resetOverlaySize 恢复悬浮窗到默认大小
func resetOverlaySize() {
	if debugHwnd == 0 {
		return
	}
	overlayW, overlayH = 240, 70
	overlayX = -1
	overlayY = -1
	sw, _, _ := procGetSystemMetrics.Call(0)
	x := int(sw)*2/3 - overlayW/2
	y := 23
	procSetWindowPos.Call(uintptr(debugHwnd), 0, uintptr(x), uintptr(y), uintptr(overlayW), uintptr(overlayH), 0x0014)
	saveOverlaySize()
}

// configDir 返回用户配置目录（%APPDATA%\客屏对接），自动创建
func configDir() string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		appData = exeDir
	}
	dir := filepath.Join(appData, "客屏对接")
	os.MkdirAll(dir, 0755)
	return dir
}

// saveOverlaySize 保存悬浮窗大小和位置到配置文件
func saveOverlaySize() {
	data := fmt.Sprintf("%dx%d,%dx%d", overlayW, overlayH, overlayX, overlayY)
	os.WriteFile(filepath.Join(configDir(), "overlay_size.txt"), []byte(data), 0644)
}

// loadOverlaySize 从配置文件加载悬浮窗大小和位置
func loadOverlaySize() {
	data, err := os.ReadFile(filepath.Join(configDir(), "overlay_size.txt"))
	if err != nil {
		data, err = os.ReadFile(filepath.Join(exeDir, "overlay_size.txt"))
		if err != nil {
			return
		}
	}
	s := strings.TrimSpace(string(data))
	// 格式：宽x高,左x顶
	parts := strings.Split(s, ",")
	var w, h, x, y int
	if _, err := fmt.Sscanf(parts[0], "%dx%d", &w, &h); err == nil {
		if w >= 150 && w <= 1200 && h >= 50 && h <= 400 {
			overlayW, overlayH = w, h
		}
	}
	if len(parts) >= 2 {
		if _, err := fmt.Sscanf(parts[1], "%dx%d", &x, &y); err == nil {
			overlayX, overlayY = x, y
		}
	}
}

// showOverlayContextMenu 显示右键菜单
func showOverlayContextMenu(hwnd windows.HWND, x, y int, screenCoords bool) {
	hMenu, _, _ := procCreatePopupMenu.Call()
	settingsText, _ := syscall.UTF16PtrFromString("客屏设置")
	procAppendMenuW.Call(hMenu, 0, ID_OVERLAY_SETTINGS, uintptr(unsafe.Pointer(settingsText)))
	resetText, _ := syscall.UTF16PtrFromString("恢复窗口")
	procAppendMenuW.Call(hMenu, 0, ID_OVERLAY_RESET, uintptr(unsafe.Pointer(resetText)))
	closeText, _ := syscall.UTF16PtrFromString("关闭窗口")
	procAppendMenuW.Call(hMenu, 0, ID_OVERLAY_CLOSE, uintptr(unsafe.Pointer(closeText)))
	if !screenCoords {
		var pt point
		pt.X = int32(x)
		pt.Y = int32(y)
		procClientToScreen.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&pt)))
		x, y = int(pt.X), int(pt.Y)
	}
	procTrackPopupMenu.Call(hMenu, 0x0002, uintptr(x), uintptr(y), 0, uintptr(hwnd), 0)
	procDestroyMenu.Call(hMenu)
}

// toggleOverlay 切换悬浮窗显示
func toggleOverlay() {
	if overlayVisible {
		hideOverlay()
	} else {
		showOverlay()
	}
}

// showOverlay 显示悬浮窗
func showOverlay() {
	overlayVisible = true
	if mOverlay != nil {
		mOverlay.Check()
	}
	if debugHwnd == 0 {
		go createDebugOverlay()
	} else {
		procShowWindow.Call(uintptr(debugHwnd), 5) // SW_SHOW
	}
}

// hideOverlay 隐藏悬浮窗
func hideOverlay() {
	overlayVisible = false
	if mOverlay != nil {
		mOverlay.Uncheck()
	}
	if debugHwnd != 0 {
		procShowWindow.Call(uintptr(debugHwnd), 0) // SW_HIDE
	}
}

// createDebugOverlay 创建始终置顶的金额悬浮窗
func createDebugOverlay() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	className, _ := syscall.UTF16PtrFromString("AmountOverlay")
	instance, _, _ := procGetModuleHandle.Call(0)

	bgBrush, _, _ := procCreateSolidBrush.Call(uintptr(overlayBgColor))

	var wc wndClassEx
	wc.Size = uint32(unsafe.Sizeof(wc))
	wc.Style = 0x0003
	wc.WndProc = syscall.NewCallback(overlayWndProc)
	wc.Instance = instance
	wc.Cursor, _, _ = procLoadCursorW.Call(0, 32512)
	wc.Background = bgBrush
	wc.ClassName = className
	procRegisterClassExW.Call(uintptr(unsafe.Pointer(&wc)))

	loadOverlaySize()
	title, _ := syscall.UTF16PtrFromString("客显金额")
	// 默认位置：屏幕宽约63%处，顶部留23px（与当前位置一致）
	sw, _, _ := procGetSystemMetrics.Call(0)
	ovW, ovH := overlayW, overlayH
	x := int(sw)*2/3 - ovW/2
	y := 23
	if overlayX >= 0 {
		x = overlayX
		y = overlayY
	}
	hwndPtr, _, _ := procCreateWindowExW.Call(
		0x00000008|0x00000080, // WS_EX_TOPMOST | WS_EX_TOOLWINDOW
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(title)),
		0x80000000|0x80000000, // WS_POPUP | WS_VISIBLE（无边框，WM_NCHITTEST处理调整大小）
		uintptr(x), uintptr(y), uintptr(ovW), uintptr(ovH),
		0, 0, instance, 0,
	)
	debugHwnd = windows.HWND(hwndPtr)

	// 圆角窗口
	hrgn, _, _ := procCreateRoundRectRgn.Call(0, 0, uintptr(ovW+1), uintptr(ovH+1), 16, 16)
	procSetWindowRgn.Call(uintptr(debugHwnd), hrgn, 1)

	// 金额显示静态文本（居中，垂直居中）
	stText, _ := syscall.UTF16PtrFromString("STATIC")
	amountText, _ := syscall.UTF16PtrFromString("等待识别...")
	staticPtr, _, _ := procCreateWindowExW.Call(
		0,
		uintptr(unsafe.Pointer(stText)),
		uintptr(unsafe.Pointer(amountText)),
		0x50000000|0x00000001|0x00000200, // WS_CHILD|WS_VISIBLE | SS_CENTER | SS_CENTERIMAGE
		0, uintptr(int(float64(ovH)*0.10)), uintptr(ovW), uintptr(int(float64(ovH)*0.80)),
		uintptr(debugHwnd), 0, instance, 0,
	)
	debugStatic = windows.HWND(staticPtr)

	// 根据窗口高度自适应字体大小
	applyOverlayFont()

	procShowWindow.Call(uintptr(debugHwnd), 5)
	procUpdateWindow.Call(uintptr(debugHwnd))

	var m winMsg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if ret == 0 {
			break
		}
		procTranslateMessage.Call(uintptr(unsafe.Pointer(&m)))
		procDispatchMessageW.Call(uintptr(unsafe.Pointer(&m)))
	}
}

// updateDebugOverlay 更新悬浮窗显示的金额
func updateDebugOverlay(amount string) {
	if debugHwnd == 0 || debugStatic == 0 {
		return
	}
	displayText := "RM  " + amount
	setWindowText(debugStatic, displayText)
	procInvalidateRect.Call(uintptr(debugHwnd), 0, 1)
}

// EnumWindows全局callback（只创建一次，避免too many callback functions崩溃）
var enumCandidates []enumCandidate
var enumTitle string

var enumCallback = syscall.NewCallback(func(hwnd windows.HWND, lparam uintptr) uintptr {
	ret, _, _ := procIsWindowVisible.Call(uintptr(hwnd))
	if ret == 0 {
		return 1
	}
	classBuf := make([]uint16, 256)
	procGetClassNameW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&classBuf[0])), 256)
	className := windows.UTF16ToString(classBuf)
	if !strings.Contains(strings.ToLower(className), "flutter") {
		return 1
	}
	buf := make([]uint16, 256)
	procGetWindowTextW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&buf[0])), 256)
	t := windows.UTF16ToString(buf)
	var r rect
	procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r)))
	w, h := r.Right-r.Left, r.Bottom-r.Top
	if strings.Contains(t, enumTitle) {
		minimized := false
		if w <= 100 || h <= 100 {
			var wp windowPlacement
			wp.Length = uint32(unsafe.Sizeof(wp))
			if ret2, _, _ := procGetWindowPlacement.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&wp))); ret2 != 0 && wp.ShowCmd == 2 {
				minimized = true
				w = wp.RcNormalPosition.Right - wp.RcNormalPosition.Left
				h = wp.RcNormalPosition.Bottom - wp.RcNormalPosition.Top
			}
		}
		if w > 100 && h > 100 {
			enumCandidates = append(enumCandidates, enumCandidate{hwnd, minimized, className})
		}
	}
	return 1
})

type enumCandidate struct {
	hwnd      windows.HWND
	minimized bool
	className string
}

func findWindow(title string) windows.HWND {
	enumTitle = title
	enumCandidates = nil
	procEnumWindows.Call(enumCallback, 0)

	if len(enumCandidates) == 0 {
		return 0
	}

	// 优先选 FlutterMultiWindow 类窗口（云零售主窗口）
	for _, c := range enumCandidates {
		if c.className == "FlutterMultiWindow" {
			return c.hwnd
		}
	}
	// 其次选最小化的窗口
	for _, c := range enumCandidates {
		if c.minimized {
			return c.hwnd
		}
	}
	// 都没有就选第一个
	return enumCandidates[0].hwnd
}

func captureWindow(hwnd windows.HWND) *image.RGBA {
	var r rect
	procGetWindowRect.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&r)))
	w, h := int(r.Right-r.Left), int(r.Bottom-r.Top)
	// 最小化窗口 GetWindowRect 返回任务栏按钮尺寸，用 GetWindowPlacement 拿正常尺寸
	if w < 100 || h < 100 {
		var wp windowPlacement
		wp.Length = uint32(unsafe.Sizeof(wp))
		if ret, _, _ := procGetWindowPlacement.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&wp))); ret != 0 && wp.ShowCmd == 2 {
			w = int(wp.RcNormalPosition.Right - wp.RcNormalPosition.Left)
			h = int(wp.RcNormalPosition.Bottom - wp.RcNormalPosition.Top)
		}
	}
	if w < 100 || h < 100 {
		return nil
	}

	hdc, _, _ := procGetWindowDC.Call(uintptr(hwnd))
	memDC, _, _ := procCreateCompatibleDC.Call(hdc)

	// 用 CreateDIBSection 创建可直接访问像素内存的位图
	var bi bitmapInfoHeader
	bi.Size = uint32(unsafe.Sizeof(bi))
	bi.Width = int32(w)
	bi.Height = -int32(h)
	bi.Planes = 1
	bi.BitCount = 32
	var bits unsafe.Pointer
	hBmp, _, _ := procCreateDIBSection.Call(memDC, uintptr(unsafe.Pointer(&bi)), 0, uintptr(unsafe.Pointer(&bits)), 0, 0)
	procSelectObject.Call(memDC, hBmp)

	// 用 PrintWindow 截窗口自己的内容（配合 DIB 直接读像素，避免 GetDIBits 全白问题）
	procPrintWindow.Call(uintptr(hwnd), memDC, 2) // PW_RENDERFULLCONTENT

	// 从 DIB 像素内存复制到 image.RGBA
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	pixelCount := w * h * 4
	src := (*[1 << 30]byte)(bits)[:pixelCount:pixelCount]
	copy(img.Pix, src)

	procDeleteObject.Call(hBmp)
	procDeleteDC.Call(memDC)
	procReleaseDC.Call(uintptr(hwnd), hdc)

	return img
}

func ocrAmount(img *image.RGBA) string {
	w := img.Bounds().Dx()
	h := img.Bounds().Dy()

	// 检测窗口尺寸变化（全屏/窗口化切换），切换中跳过OCR保持上次结果
	if w != lastW || h != lastH {
		lastW, lastH = w, h
		switching = true
		stableCount = 0
		return lastOCR
	}
	if switching {
		stableCount++
		if stableCount >= 3 {
			switching = false
			stableCount = 0
		} else {
			return lastOCR
		}
	}

	// 数字模板匹配（纯内存，无磁盘写入）
	if tmplLoaded {
		result := matchAmountTemplate(img)
		if result != "" {
			lastOCR = result
		}
		return result
	}
	return ""
}

// ========== 模板匹配OCR ==========

// normalizeTemplate 重新归一化模板：找到黑色像素边界，按固定高度缩放
func normalizeTemplate(src *image.Gray) *image.Gray {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	minX, minY, maxX, maxY := w, h, 0, 0
	found := false
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			if src.GrayAt(x, y).Y == 0 {
				if x < minX { minX = x }
				if y < minY { minY = y }
				if x > maxX { maxX = x }
				if y > maxY { maxY = y }
				found = true
			}
		}
	}
	if !found {
		return src
	}
	r := charRegion{minX, maxX + 1, minY, maxY + 1}
	return extractCharTemplate(src, r, tmplStdW, tmplStdH)
}

func loadTemplateSet(fs embed.FS, dir string) map[string]*image.Gray {
	m := make(map[string]*image.Gray)
	for _, c := range "0123456789" {
		path := fmt.Sprintf("%s/char_%c.png", dir, c)
		if t := loadGrayPNGFromFS(fs, path); t != nil {
			m[string(c)] = normalizeTemplate(t)
		}
	}
	if t := loadGrayPNGFromFS(fs, dir+"/char_dot.png"); t != nil {
		m["."] = normalizeTemplate(t)
	}
	if t := loadGrayPNGFromFS(fs, dir+"/char_yen.png"); t != nil {
		m["¥"] = normalizeTemplate(t)
	}
	return m
}

func loadTemplates() {
	tmplMap = loadTemplateSet(templateFS, "templates")
	tmplMapHalf = loadTemplateSet(templateHalfFS, "templates_half")
	tmplLoaded = len(tmplMap) > 0
	log.Printf("模板匹配: 全屏模板%d个, 半屏模板%d个", len(tmplMap), len(tmplMapHalf))
}

func loadGrayPNGFromFS(fs embed.FS, path string) *image.Gray {
	f, err := fs.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil
	}
	bounds := img.Bounds()
	gray := image.NewGray(bounds)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			r, g, b, _ := img.At(x, y).RGBA()
			gv := uint8((r*299 + g*587 + b*114) / 1000 / 256)
			gray.SetGray(x, y, color.Gray{Y: gv})
		}
	}
	return gray
}

// binarizeToGray 黑底白字按钮二值化为白底黑字
func binarizeToGray(src *image.RGBA) *image.Gray {
	w, h := src.Bounds().Dx(), src.Bounds().Dy()
	out := image.NewGray(image.Rect(0, 0, w, h))
	minX, minY := src.Bounds().Min.X, src.Bounds().Min.Y
	for y := 0; y < h; y++ {
		srcOff := src.PixOffset(minX, minY+y)
		dstOff := y * w
		for x := 0; x < w; x++ {
			r := uint32(src.Pix[srcOff+x*4])
			g := uint32(src.Pix[srcOff+x*4+1])
			b := uint32(src.Pix[srcOff+x*4+2])
			gray := (r*299 + g*587 + b*114) / 1000
			if gray > 150 {
				out.Pix[dstOff+x] = 0 // 白字→黑
			} else {
				out.Pix[dstOff+x] = 255 // 黑底→白
			}
		}
	}
	return out
}

func cropGrayImg(img *image.Gray, x1, y1, x2, y2 int) *image.Gray {
	if x1 < 0 { x1 = 0 }
	if y1 < 0 { y1 = 0 }
	if x2 > img.Bounds().Dx() { x2 = img.Bounds().Dx() }
	if y2 > img.Bounds().Dy() { y2 = img.Bounds().Dy() }
	w, h := x2-x1, y2-y1
	out := image.NewGray(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			out.SetGray(x, y, img.GrayAt(x1+x, y1+y))
		}
	}
	return out
}

type charRegion struct {
	x1, x2, y1, y2 int
}

func segmentComponents(img *image.Gray) []charRegion {
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	visited := make([]bool, w*h)
	var regions []charRegion
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			idx := y*w + x
			if visited[idx] || img.GrayAt(x, y).Y != 0 {
				continue
			}
			minX, maxX, minY, maxY := x, x, y, y
			queue := [][2]int{{x, y}}
			visited[idx] = true
			for len(queue) > 0 {
				cx, cy := queue[0][0], queue[0][1]
				queue = queue[1:]
				if cx < minX { minX = cx }
				if cx > maxX { maxX = cx }
				if cy < minY { minY = cy }
				if cy > maxY { maxY = cy }
				for dy := -1; dy <= 1; dy++ {
					for dx := -1; dx <= 1; dx++ {
						nx, ny := cx+dx, cy+dy
						if nx >= 0 && nx < w && ny >= 0 && ny < h {
							nidx := ny*w + nx
							if !visited[nidx] && img.GrayAt(nx, ny).Y == 0 {
								visited[nidx] = true
								queue = append(queue, [2]int{nx, ny})
							}
						}
					}
				}
			}
			rw, rh := maxX-minX, maxY-minY
			if rw >= 1 && rw < 80 && rh >= 2 && rh < 60 {
				regions = append(regions, charRegion{minX, maxX + 1, minY, maxY + 1})
			}
		}
	}
	sort.Slice(regions, func(i, j int) bool { return regions[i].x1 < regions[j].x1 })
	return regions
}

func extractCharTemplate(img *image.Gray, r charRegion, stdW, stdH int) *image.Gray {
	cw, ch := r.x2-r.x1, r.y2-r.y1
	if cw <= 0 || ch <= 0 {
		return image.NewGray(image.Rect(0, 0, stdW, stdH))
	}
	// 按固定高度缩放，保持原始宽度比例
	scale := float64(stdH) / float64(ch)
	dstW := int(float64(cw) * scale)
	if dstW > stdW {
		dstW = stdW
	}
	dstH := stdH
	offsetX := (stdW - dstW) / 2
	out := image.NewGray(image.Rect(0, 0, stdW, stdH))
	for i := range out.Pix {
		out.Pix[i] = 255 // 初始化为白色
	}
	for y := 0; y < dstH; y++ {
		srcY := r.y1 + int(float64(y)/scale)
		if srcY >= r.y2 {
			continue
		}
		dstOff := y*stdW + offsetX
		for x := 0; x < dstW; x++ {
			srcX := r.x1 + int(float64(x)/scale)
			if srcX < r.x2 {
				out.Pix[dstOff+x] = img.Pix[srcY*img.Stride+srcX]
			}
		}
	}
	return out
}

func similarityTemplate(a, b *image.Gray) float64 {
	w, h := a.Bounds().Dx(), a.Bounds().Dy()
	if b.Bounds().Dx() != w || b.Bounds().Dy() != h {
		return 0
	}
	intersection := 0 // 两个都是黑色
	union := 0        // 至少一个是黑色
	for y := 0; y < h; y++ {
		off := y * w
		for x := 0; x < w; x++ {
			ab := a.Pix[off+x] == 0
			bb := b.Pix[off+x] == 0
			if ab || bb {
				union++
				if ab && bb {
					intersection++
				}
			}
		}
	}
	if union == 0 {
		return 0
	}
	return float64(intersection) / float64(union)
}

// matchAmountTemplate 用模板匹配识别金额
func matchAmountTemplate(img *image.RGBA) string {
	if !tmplLoaded {
		return ""
	}
	w, h := img.Bounds().Dx(), img.Bounds().Dy()
	// 根据窗口高度选择模板集：全屏>900px用全屏模板，否则用半屏模板
	activeTmpl := tmplMap
	if h <= 900 && len(tmplMapHalf) > 0 {
		activeTmpl = tmplMapHalf
	}
	// 裁剪底部按钮行（按比例）
	btnW := int(float64(w) * 0.30)
	btnH := int(float64(h) * 0.075)
	if btnH > h { btnH = h }
	if btnW > w { btnW = w }
	y0 := h - btnH
	y1 := h - int(float64(btnH)*0.15)
	if y1 <= y0 {
		return ""
	}
	crop := img.SubImage(image.Rect(0, y0, btnW, y1)).(*image.RGBA)
	// 二值化
	bin := binarizeToGray(crop)
	// 去边框
	bin = cropGrayImg(bin, 15, 10, bin.Bounds().Dx()-15, bin.Bounds().Dy()-8)
	// 连通域分割
	regions := segmentComponents(bin)
	if len(regions) == 0 {
		return ""
	}
	// 匹配所有字符，用¥做锚点，只取¥后面的数字
	var allChars []string
	var allScores []float64
	for _, r := range regions {
		cw, ch := r.x2-r.x1, r.y2-r.y1
		charImg := extractCharTemplate(bin, r, tmplStdW, tmplStdH)
		bestChar := "?"
		bestScore := 0.0
		// 小尺寸连通域直接判定为小数点（宽<12且高<15）
		if cw < 12 && ch < 15 {
			bestChar = "."
			bestScore = 0.90
		} else if float64(cw)/float64(ch) < 0.60 {
			// 窄字符判定为"1"（宽高比<0.60，"1"约0.48-0.56，"3"约0.68+）
			bestChar = "1"
			bestScore = 0.85
		} else {
			for c, t := range activeTmpl {
				if c == "." {
					continue // 小数点不用模板匹配
				}
				s := similarityTemplate(charImg, t)
				if s > bestScore {
					bestScore = s
					bestChar = c
				}
			}
			// 区分"0"和"6"：比较左上/右上象限黑色像素比例
			// "6"左上象限有尾巴，像素远多于右上；"0"上下左右对称
			if bestChar == "0" {
				w := charImg.Bounds().Dx()
				h := charImg.Bounds().Dy()
				leftTop, rightTop := 0, 0
				halfW := w / 2
				for y := 0; y < h/2; y++ {
					off := y * w
					for x := 0; x < halfW; x++ {
						if charImg.Pix[off+x] == 0 {
							leftTop++
						}
					}
					for x := halfW; x < w; x++ {
						if charImg.Pix[off+x] == 0 {
							rightTop++
						}
					}
				}
				ratio := float64(leftTop) / float64(rightTop+1)
				// 左上像素是右上1.3倍以上 → "6"的尾巴
				if ratio > 1.3 {
					bestChar = "6"
					if t, ok := activeTmpl["6"]; ok {
						bestScore = similarityTemplate(charImg, t)
					}
				}
				if debugMode {
					log.Printf("0/6判定: 左上=%d 右上=%d 比例=%.2f → %s", leftTop, rightTop, ratio, bestChar)
				}
			}
			// 区分"3"和"8"：比较中间行左右半像素比例
			// "3"中间左侧开口（像素少），右侧圆弧（像素多）；"8"左右闭合（像素接近）
			if bestChar == "8" {
				w := charImg.Bounds().Dx()
				h := charImg.Bounds().Dy()
				midLeft, midRight := 0, 0
				midY := h / 2
				halfW := w / 2
				for y := midY - 3; y <= midY+3; y++ {
					if y < 0 || y >= h {
						continue
					}
					off := y * w
					for x := 0; x < halfW; x++ {
						if charImg.Pix[off+x] == 0 {
							midLeft++
						}
					}
					for x := halfW; x < w; x++ {
						if charImg.Pix[off+x] == 0 {
							midRight++
						}
					}
				}
				ratio := float64(midLeft) / float64(midRight+1)
				if debugMode {
					log.Printf("3/8判定: 左=%d 右=%d 比例=%.2f → %s", midLeft, midRight, ratio, bestChar)
				}
				// 左/右 < 0.75 → "3"（左侧开口）
				if ratio < 0.75 {
					if t, ok := activeTmpl["3"]; ok {
						bestChar = "3"
						bestScore = similarityTemplate(charImg, t)
					}
				}
			}
		}
		allChars = append(allChars, bestChar)
		allScores = append(allScores, bestScore)
	}
	// 找¥的位置，只取后面的字符
	yenIdx := -1
	for i, c := range allChars {
		if c == "¥" && allScores[i] > 0.35 {
			yenIdx = i
			break
		}
	}
	var result strings.Builder
	startIdx := 0
	if yenIdx >= 0 {
		startIdx = yenIdx + 1
	}
	for i := startIdx; i < len(allChars); i++ {
		if allScores[i] > 0.40 && allChars[i] != "¥" {
			r := regions[i]
			ch := r.y2 - r.y1
			// 过滤噪点：非小数点字符高度必须>10px（真实数字18-29px，噪点<8px）
			if allChars[i] != "." && ch < 10 {
				continue
			}
			result.WriteString(allChars[i])
		}
	}
	raw := result.String()
	// 调试日志
	debugStr := ""
	for i, c := range allChars {
		r := regions[i]
		debugStr += fmt.Sprintf("%s(%.2f,%dx%d@%d) ", c, allScores[i], r.x2-r.x1, r.y2-r.y1, r.x1)
	}
	if debugMode {
		log.Printf("模板调试: 连通域=%d ¥位置=%d 字符=[%s] 原始=%s", len(regions), yenIdx, debugStr, raw)
	}
	// 格式化金额
	nums := amountRe.FindAllString(raw, -1)
	if len(nums) > 0 {
		if f, err := strconv.ParseFloat(nums[0], 64); err == nil {
			return fmt.Sprintf("%.2f", f)
		}
		return nums[0]
	}
	return ""
}
