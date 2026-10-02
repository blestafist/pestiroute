package crypto

import (
	"io"
	"os"
	"syscall"
)

// LoadMasterKey reads an operator-provisioned, owner-only 32-byte key file.
// Its descriptor checks use Linux syscalls; other operating systems are unsupported.
func LoadMasterKey(path string) (MasterKey, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_CLOEXEC|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return MasterKey{}, ErrKeyFile
	}
	f := os.NewFile(uintptr(fd), "master-key")
	defer f.Close()

	var stat syscall.Stat_t
	if syscall.Fstat(fd, &stat) != nil || stat.Mode&syscall.S_IFMT != syscall.S_IFREG || stat.Uid != uint32(os.Geteuid()) || stat.Mode&07777 != 0600 || stat.Size != KeySize {
		return MasterKey{}, ErrKeyFile
	}
	var key MasterKey
	if _, err := io.ReadFull(f, key.value[:]); err != nil {
		return MasterKey{}, ErrKeyFile
	}
	var extra [1]byte
	if n, err := f.Read(extra[:]); n != 0 || err != io.EOF {
		return MasterKey{}, ErrKeyFile
	}
	return key, nil
}
