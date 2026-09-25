// Package sysinfo reads volume-level disk usage via statfs.
package sysinfo

import (
	"golang.org/x/sys/unix"
)

// Volume describes one mounted filesystem.
type Volume struct {
	Name       string // mount point, e.g. "/System/Volumes/Data" displayed as "Macintosh HD"
	Mount      string
	TotalBytes uint64
	FreeBytes  uint64
}

// UsedBytes returns total minus free.
func (v Volume) UsedBytes() uint64 { return v.TotalBytes - v.FreeBytes }

// RootVolume stats the volume backing the user's data (the root mount on
// modern macOS is the data volume).
func RootVolume() (Volume, error) {
	return VolumeAt("/")
}

// VolumeAt stats the volume containing path.
func VolumeAt(path string) (Volume, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(path, &st); err != nil {
		return Volume{}, err
	}
	return Volume{
		Name:       volumeName(path),
		Mount:      path,
		TotalBytes: uint64(st.Bsize) * uint64(st.Blocks),
		FreeBytes:  uint64(st.Bsize) * uint64(st.Bavail),
	}, nil
}

func volumeName(path string) string {
	if path == "/" {
		return "Macintosh HD"
	}
	return path
}
