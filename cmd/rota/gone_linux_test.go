package main

import "testing"

// Where the kernel has no pidfd to give — before Linux 5.3, under a seccomp
// filter that refuses it, on MIPS — exitOf looks at /proc instead, and
// answers the same.
func TestExitOfAnswersByLookingWithoutAPidfd(t *testing.T) {
	lookOnly = true
	t.Cleanup(func() { lookOnly = false })
	exitOfAnswers(t)
}
