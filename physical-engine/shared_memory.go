// shared_memory_windows.go
//go:build windows
// +build windows

package physicalengine

import (
	"fmt"
	"log"
	"syscall"
	"unsafe"
)

// Windows共享内存结构体
type SharedMemoryData struct {
	PlayerID     uint32
	PositionX    float32
	PositionY    float32
	VelocityX    float32
	VelocityY    float32
	InputX       float32
	InputY       float32
	IsJumping    uint32 // Windows下使用uint32代替bool
	IsConnected  uint32
	FrameCounter uint32
}

type OperationSharedMemoryData struct {
	PlayerID    uint32  // 玩家ID
	InputX      float32 // 水平输入 (-1到1)
	InputY      float32 // 垂直输入 (-1到1)
	IsJumping   uint32  // 是否跳跃
	IsConnected uint32  // 是否连接
}

type PhysicsSharedMemoryData struct {
	PlayerID     uint32  // 玩家ID
	PositionX    float32 // X位置
	PositionY    float32 // Y位置
	VelocityX    float32 // X速度
	VelocityY    float32 // Y速度
	FrameCounter uint32  // 帧计数器
}

type SharedMemoryManager struct {
	shmName  string
	shmSize  int
	hMapFile syscall.Handle
	shmPtr   uintptr
	shmData  *SharedMemoryData
	isOwner  bool
	OorP     bool //OperationSharedMemoryData or PhysicsSharedMemoryData
}

// Windows共享内存常量
var (
	modkernel32 = syscall.NewLazyDLL("kernel32.dll")

	procCreateFileMappingW = modkernel32.NewProc("CreateFileMappingW")
	procMapViewOfFile      = modkernel32.NewProc("MapViewOfFile")
	procUnmapViewOfFile    = modkernel32.NewProc("UnmapViewOfFile")
	procCloseHandle        = modkernel32.NewProc("CloseHandle")
)

const (
	PAGE_READWRITE       = 0x04
	FILE_MAP_ALL_ACCESS  = 0xF001F
	INVALID_HANDLE_VALUE = ^syscall.Handle(0)
)

func NewSharedMemoryManager(name string, size int, create bool, OorP bool) (*SharedMemoryManager, error) {
	mgr := &SharedMemoryManager{
		shmName: name,
		shmSize: size,
		isOwner: create,
		OorP:    OorP,
	}

	// 转换名称为UTF-16
	namePtr, err := syscall.UTF16PtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("名称转换失败: %v", err)
	}

	if create {
		// 创建共享内存
		hMap, _, err := procCreateFileMappingW.Call(
			uintptr(INVALID_HANDLE_VALUE), // 使用系统页面文件
			0,                             // 安全属性
			uintptr(PAGE_READWRITE),       // 保护模式
			0,                             // 高位大小
			uintptr(size),                 // 低位大小
			uintptr(unsafe.Pointer(namePtr)),
		)

		if hMap == 0 {
			return nil, fmt.Errorf("创建共享内存失败: %v", err)
		}
		mgr.hMapFile = syscall.Handle(hMap)
	} else {
		// 打开现有共享内存
		hMap, _, err := procCreateFileMappingW.Call(
			uintptr(INVALID_HANDLE_VALUE),
			0,
			uintptr(PAGE_READWRITE),
			0,
			uintptr(size),
			uintptr(unsafe.Pointer(namePtr)),
		)

		if hMap == 0 {
			return nil, fmt.Errorf("打开共享内存失败: %v", err)
		}
		mgr.hMapFile = syscall.Handle(hMap)
	}

	// 映射共享内存视图
	ptr, _, err := procMapViewOfFile.Call(
		uintptr(mgr.hMapFile),
		uintptr(FILE_MAP_ALL_ACCESS),
		0,
		0,
		uintptr(size),
	)

	if ptr == 0 {
		procCloseHandle.Call(uintptr(mgr.hMapFile))
		return nil, fmt.Errorf("映射共享内存失败: %v", err)
	}

	mgr.shmPtr = ptr
	mgr.shmData = (*SharedMemoryData)(unsafe.Pointer(ptr))

	if create {
		mgr.Initialize()
	}

	log.Printf("Windows共享内存 %s 已%s", name, map[bool]string{true: "创建", false: "打开"}[create])
	return mgr, nil
}

func (mgr *SharedMemoryManager) Initialize() {
	*mgr.shmData = SharedMemoryData{
		PlayerID:     1,
		PositionX:    0,
		PositionY:    0,
		VelocityX:    0,
		VelocityY:    0,
		InputX:       0,
		InputY:       0,
		IsJumping:    0,
		IsConnected:  1,
		FrameCounter: 0,
	}
}

func (mgr *SharedMemoryManager) ReadInput() (float32, float32, bool) {
	//log.Printf("ReadInput: %f, %f, %t", mgr.shmData.InputX, mgr.shmData.InputY, mgr.shmData.IsJumping != 0)
	return mgr.shmData.InputX, mgr.shmData.InputY, mgr.shmData.IsJumping != 0
}

func (mgr *SharedMemoryManager) WritePhysics(x, y, vx, vy float32) {
	mgr.shmData.PositionX = x
	mgr.shmData.PositionY = y
	mgr.shmData.VelocityX = vx
	mgr.shmData.VelocityY = vy
	mgr.shmData.FrameCounter++
}

func (mgr *SharedMemoryManager) Close() {
	if mgr.shmPtr != 0 {
		procUnmapViewOfFile.Call(mgr.shmPtr)
	}
	if mgr.hMapFile != 0 {
		procCloseHandle.Call(uintptr(mgr.hMapFile))
	}
	log.Printf("Windows共享内存 %s 已关闭", mgr.shmName)
}

func (mgr *SharedMemoryManager) ShowSharedMemory() {

	log.Printf("SharedMemoryData: %+v", *mgr.shmData)

}
