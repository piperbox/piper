package main

import (
	"testing"
)

func stubExec(t *testing.T) (execs *[][]string, exits *[]int) {
	t.Helper()
	var e [][]string
	var x []int
	oldExec, oldExit := execFn, exitFn
	execFn = func(argv0 string, argv []string, envv []string) error {
		e = append(e, append([]string{argv0}, argv...))
		return nil
	}
	exitFn = func(code int) { x = append(x, code) }
	t.Cleanup(func() { execFn, exitFn = oldExec, oldExit })
	return &e, &x
}

func TestExecSelfRunsWhenBinaryUnchanged(t *testing.T) {
	execs, exits := stubExec(t)
	captureBootExe()
	execSelf()
	if len(*exits) != 0 {
		t.Fatalf("exited %v instead of exec", *exits)
	}
	if len(*execs) != 1 {
		t.Fatalf("execs = %v", *execs)
	}
}

func TestExecSelfRefusesWhenBinaryChanged(t *testing.T) {
	execs, exits := stubExec(t)
	captureBootExe()
	old := bootExe
	bootExe.ino = old.ino + 1 // simulate a replaced binary
	t.Cleanup(func() { bootExe = old })
	execSelf()
	if len(*execs) != 0 {
		t.Fatal("exec'd a binary that changed since boot")
	}
	if len(*exits) != 1 || (*exits)[0] != 1 {
		t.Fatalf("exits = %v, want [1] (supervised restart)", *exits)
	}
}

func TestExecSelfExitsOneWhenIdentityUnknown(t *testing.T) {
	_, exits := stubExec(t)
	old := bootExe
	bootExe.ok = false
	t.Cleanup(func() { bootExe = old })
	execSelf()
	if len(*exits) != 1 || (*exits)[0] != 1 {
		t.Fatalf("exits = %v, want [1]", *exits)
	}
}
