//go:build linux
// +build linux

package physicalengine

import (
	"log"
)

// Linux共享内存结构体（与Windows版本相同）
type SharedMemoryData struct {
	PlayerID     [32]byte
	PositionX    float32
	PositionY    float32
	VelocityX    float32
	VelocityY    float32
	InputX       float32
	InputY       float32
	IsJumping    uint32
	IsConnected  uint32
	FrameCounter uint32
}

type SharedMemoryManager struct {
	shmName string
	shmSize int
	shmFd   int
	shmPtr  uintptr
	shmData *SharedMemoryData
	isOwner bool
}

// Linux共享内存实现
func NewSharedMemoryManager(name string, size int, create bool) (*SharedMemoryManager, error) {
	// 在Linux上返回空实现，避免编译错误
	log.Printf("Linux共享内存功能暂未实现，使用空实现")

	mgr := &SharedMemoryManager{
		shmName: name,
		shmSize: size,
		isOwner: create,
	}

	return mgr, nil
}

func (mgr *SharedMemoryManager) Initialize() {
	// 空实现
}

func (mgr *SharedMemoryManager) ReadInput() (float32, float32, bool) {
	return 0, 0, false
}

func (mgr *SharedMemoryManager) WritePhysics(x, y, vx, vy float32) {
	// 空实现
}

func (mgr *SharedMemoryManager) Close() {
	// 空实现
}
