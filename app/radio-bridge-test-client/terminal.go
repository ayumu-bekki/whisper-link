package main

import (
	"golang.org/x/sys/unix"
)

// makeRaw はターミナルをrawモードにしてキー入力をバッファリングなしで取得できるようにする。
func makeRaw(fd int) (*unix.Termios, error) {
	termios, err := unix.IoctlGetTermios(fd, unix.TIOCGETA)
	if err != nil {
		return nil, err
	}
	old := *termios

	// 入力のみrawモード: キー入力をバッファリングなし・エコーなしで取得する。
	// OPOST はオフにしない (出力の \n -> \r\n 変換はターミナルに任せる)。
	termios.Iflag &^= unix.IGNBRK | unix.BRKINT | unix.PARMRK | unix.ISTRIP |
		unix.INLCR | unix.IGNCR | unix.ICRNL | unix.IXON
	termios.Lflag &^= unix.ECHO | unix.ECHONL | unix.ICANON | unix.ISIG | unix.IEXTEN
	termios.Cflag &^= unix.CSIZE | unix.PARENB
	termios.Cflag |= unix.CS8
	termios.Cc[unix.VMIN] = 1
	termios.Cc[unix.VTIME] = 0

	if err := unix.IoctlSetTermios(fd, unix.TIOCSETA, termios); err != nil {
		return nil, err
	}
	return &old, nil
}

// restoreTerminal はターミナルを元の状態に戻す。
func restoreTerminal(fd int, old *unix.Termios) {
	unix.IoctlSetTermios(fd, unix.TIOCSETA, old)
}
