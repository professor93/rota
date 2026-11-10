package attach

// DaemonKey is the chord key of the daemon whose terminals directory is
// terminals, the key every attach to its sessions reads, local and SSH
// alike. In 1.4.0 the daemon leaves a session on Ctrl-\ alone, so it is
// always DefaultKey; the directory is taken so that a later daemon which
// lets the key be set can say which one it uses, with no caller changing.
func DaemonKey(terminals string) byte {
	return DefaultKey
}
