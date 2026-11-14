package crashpoint

import (
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
)

func TestMain(m *testing.M) {
	if os.Getenv("CRASHPOINT_TEST_CHILD") == "1" {
		runChild()
	}
	os.Exit(m.Run())
}

var childPoint = Register("crashpoint.test.child")

// runChild arms from the environment, hits the point and reports survival. A
// correct run is killed before printing.
func runChild() {
	armedOK, err := ArmFromEnv()
	if err != nil || !armedOK {
		os.Stdout.WriteString("arm failed\n")
		os.Exit(2)
	}
	childPoint.Hit()
	os.Stdout.WriteString("survived\n")
	os.Exit(0)
}

func reset(t *testing.T) {
	t.Helper()
	Disarm()
	t.Cleanup(Disarm)
}

func TestHit(t *testing.T) {
	t.Run("should do nothing when no point is armed", func(t *testing.T) {
		reset(t)
		p := Register("crashpoint.test.unarmed")
		p.Hit()
		p.Hit()
	})

	t.Run("should call fn on the second hit when armed with n=2", func(t *testing.T) {
		reset(t)
		p := Register("crashpoint.test.second")
		calls := 0
		Arm("crashpoint.test.second", 2, func() { calls++ })
		p.Hit()
		if calls != 0 {
			t.Fatalf("fn ran on hit 1")
		}
		p.Hit()
		if calls != 1 {
			t.Fatalf("calls = %d after hit 2, want 1", calls)
		}
		p.Hit()
		if calls != 1 {
			t.Fatalf("calls = %d after hit 3, want fn to fire exactly once", calls)
		}
	})

	t.Run("should not fire a different point when only one name is armed", func(t *testing.T) {
		reset(t)
		a := Register("crashpoint.test.a")
		b := Register("crashpoint.test.b")
		fired := ""
		Arm("crashpoint.test.a", 1, func() { fired += "a" })
		b.Hit()
		a.Hit()
		if fired != "a" {
			t.Fatalf("fired = %q, want a", fired)
		}
	})

	t.Run("should stop firing when Disarm is called", func(t *testing.T) {
		reset(t)
		p := Register("crashpoint.test.disarm")
		Arm("crashpoint.test.disarm", 1, func() { t.Error("fired after Disarm") })
		Disarm()
		p.Hit()
	})
}

func TestRegister(t *testing.T) {
	t.Run("should panic when a name is registered twice", func(t *testing.T) {
		Register("crashpoint.test.dup")
		defer func() {
			if recover() == nil {
				t.Fatal("second Register did not panic")
			}
		}()
		Register("crashpoint.test.dup")
	})

	t.Run("should list names sorted when Names is called", func(t *testing.T) {
		Register("crashpoint.test.zzz")
		Register("crashpoint.test.aaa")
		names := Names()
		for i := 1; i < len(names); i++ {
			if names[i-1] >= names[i] {
				t.Fatalf("Names not strictly sorted at %d: %v", i, names)
			}
		}
	})

	t.Run("should panic when Arm names an unregistered point", func(t *testing.T) {
		reset(t)
		defer func() {
			if recover() == nil {
				t.Fatal("Arm did not panic")
			}
		}()
		Arm("crashpoint.test.nonexistent", 1, func() {})
	})
}

func TestArmFromEnv(t *testing.T) {
	t.Run("should report not armed when COLDSTORE_CRASHPOINT is unset", func(t *testing.T) {
		reset(t)
		t.Setenv(EnvPoint, "")
		armedOK, err := ArmFromEnv()
		if armedOK || err != nil {
			t.Fatalf("ArmFromEnv = %v, %v", armedOK, err)
		}
	})

	t.Run("should return an error when COLDSTORE_CRASHPOINT names an unknown point", func(t *testing.T) {
		reset(t)
		t.Setenv(EnvPoint, "crashpoint.test.nope")
		armedOK, err := ArmFromEnv()
		if armedOK || err == nil {
			t.Fatalf("ArmFromEnv = %v, %v; want an error", armedOK, err)
		}
	})

	t.Run("should return an error when COLDSTORE_CRASHPOINT_HIT is not a positive integer", func(t *testing.T) {
		reset(t)
		t.Setenv(EnvPoint, "crashpoint.test.child")
		for _, bad := range []string{"0", "-1", "x"} {
			t.Setenv(EnvHit, bad)
			if armedOK, err := ArmFromEnv(); armedOK || err == nil {
				t.Errorf("HIT=%q: ArmFromEnv = %v, %v; want an error", bad, armedOK, err)
			}
		}
	})

	t.Run("should survive when COLDSTORE_CRASHPOINT_HIT is higher than the number of hits", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(),
			"CRASHPOINT_TEST_CHILD=1", EnvPoint+"=crashpoint.test.child", EnvHit+"=2")
		out, err := cmd.CombinedOutput()
		if err != nil || !strings.Contains(string(out), "survived") {
			t.Fatalf("child err=%v out=%s; want it to survive one hit of an n=2 arming", err, out)
		}
	})

	t.Run("should arm Kill when COLDSTORE_CRASHPOINT names a registered point", func(t *testing.T) {
		cmd := exec.Command(os.Args[0], "-test.run=^$")
		cmd.Env = append(os.Environ(),
			"CRASHPOINT_TEST_CHILD=1", EnvPoint+"=crashpoint.test.child")
		out, err := cmd.CombinedOutput()
		exitErr, ok := err.(*exec.ExitError)
		if !ok {
			t.Fatalf("child was not killed: err=%v out=%s", err, out)
		}
		status := exitErr.Sys().(syscall.WaitStatus)
		if !status.Signaled() || status.Signal() != syscall.SIGKILL {
			t.Fatalf("child status = %v, want SIGKILL; out=%s", status, out)
		}
		if strings.Contains(string(out), "survived") {
			t.Fatal("child ran past the crash point")
		}
	})
}
