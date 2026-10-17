package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// TestCases compares the exit code of each test_cases/*.c program when
// compiled by ahoy-sea against gcc. This mirrors the tcc test philosophy:
// the compiler is only correct if its output matches the reference compiler.
func TestCases(t *testing.T) {
	seaBin := "./ccompiler"
	if _, err := os.Stat(seaBin); err != nil {
		// Fall back to the prebuilt test binary.
		seaBin = "/tmp/ccompiler_test"
	}
	files, err := filepath.Glob("test_cases/*.c")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 {
		t.Skip("no test cases found")
	}
	for _, f := range files {
		f := f
		t.Run(filepath.Base(f), func(t *testing.T) {
			seaOut := filepath.Join(os.TempDir(), "sea_"+filepath.Base(f))
			gccOut := filepath.Join(os.TempDir(), "gcc_"+filepath.Base(f))

			// Compile with sea
			cmd := exec.Command(seaBin, f, "-o", seaOut)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("sea compile failed: %v\n%s", err, out)
			}
			// Compile with gcc
			cmd = exec.Command("gcc", f, "-o", gccOut)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("gcc compile failed: %v\n%s", err, out)
			}

			// Run sea
			rsea := exec.Command(seaOut)
			rsea.Run()
			rcSea := rsea.ProcessState.ExitCode()
			// Run gcc
			rgcc := exec.Command(gccOut)
			rgcc.Run()
			rcGcc := rgcc.ProcessState.ExitCode()

			if rcSea != rcGcc {
				t.Errorf("exit code mismatch: sea=%d gcc=%d", rcSea, rcGcc)
			}
		})
	}
}

func TestBasicCompile(t *testing.T) {
	src := filepath.Join(os.TempDir(), "basic.c")
	os.WriteFile(src, []byte("#include <stdio.h>\nint main(){printf(\"hi\\n\");return 0;}\n"), 0644)
	out := filepath.Join(os.TempDir(), "basic_sea")
	seaBin := "./ccompiler"
	if _, err := os.Stat(seaBin); err != nil {
		seaBin = "/tmp/ccompiler_test"
	}
	cmd := exec.Command(seaBin, src, "-o", out)
	if outb, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("compile failed: %v\n%s", err, outb)
	}
	r := exec.Command(out)
	b, _ := r.Output()
	if string(b) != "hi\n" {
		t.Errorf("output = %q, want %q", string(b), "hi\n")
	}
	_ = fmt.Sprint
}
