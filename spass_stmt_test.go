package main

// Single-pass statement codegen tests (STMT agent).
//
// Each case compiles a small C snippet with the single-pass backend
// (`-single-pass`) and with gcc, then compares exit codes. Like TestCases,
// this mirrors the tcc philosophy: the backend is correct only if its
// output behaves like the reference compiler.
//
// These are integration tests: they need EXPR (genRVal) and MEM (convert)
// alongside spass_stmt.go. Run with: go test -run TestSpassStmt .

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func spassStmtSeaBin(t *testing.T) string {
	t.Helper()
	seaBin := "./ccompiler"
	if _, err := os.Stat(seaBin); err != nil {
		seaBin = "/tmp/ccompiler_test"
	}
	if _, err := os.Stat(seaBin); err != nil {
		t.Skip("no ccompiler binary found")
	}
	return seaBin
}

func TestSpassStmt(t *testing.T) {
	seaBin := spassStmtSeaBin(t)
	cases := []struct {
		name string
		src  string
	}{
		// if / else
		{"if-true", "int main(){int x=1; if(x) return 7; return 8;}"},
		{"if-false", "int main(){int x=0; if(x) return 7; return 8;}"},
		{"if-else", "int main(){int x=0; if(x) return 7; else return 9;}"},
		{"if-elseif", "int main(){int x=2; if(x==1) return 1; else if(x==2) return 2; else return 3;}"},
		{"if-nested", "int main(){int a=5,b=3; if(a>b){ if(a-b==2) return 11; return 12;} return 13;}"},
		{"if-no-braces", "int main(){int x=1; if(x) x=42; return x;}"},
		{"if-assign-cond", "int main(){int x=0; if(x=5) return x; return 0;}"},
		{"if-else-chain", "int main(){int x=0; if(0) x=1; else if(0) x=2; else if(1) x=3; else x=4; return x;}"},
		// while / do-while
		{"while-basic", "int main(){int i=0; while(i<10) i=i+1; return i;}"},
		{"while-zero-trip", "int main(){int i=5; while(i<0) i=99; return i;}"},
		{"while-nested", "int main(){int i=0,j,s=0; while(i<3){j=0; while(j<3){s=s+1; j=j+1;} i=i+1;} return s;}"},
		{"while-dec", "int main(){int i=10; while(i) i=i-1; return i;}"},
		{"dowhile-once", "int main(){int i=0; do { i=5; } while(0); return i;}"},
		{"dowhile-loop", "int main(){int i=0; do { i=i+1; } while(i<7); return i;}"},
		{"dowhile-continue", "int main(){int i=0,s=0; do { i=i+1; if(i<3) continue; s=s+i; } while(i<5); return s;}"},
		// for
		{"for-basic", "int main(){int s=0; for(int i=0;i<10;i=i+1) s=s+i; return s;}"},
		{"for-empty-cond", "int main(){int i=0; for(;i<5;) i=i+1; return i;}"},
		{"for-infinite-break", "int main(){int i=0; for(;;){ if(i>=4) break; i=i+1; } return i;}"},
		{"for-no-init", "int main(){int i=0,s=0; for(;i<5;i=i+1) s=s+2; return i+s;}"},
		{"for-expr-init", "int main(){int i=0,s=0; for(i=1;i<=4;i=i+1) s=s+i; return s;}"},
		{"for-post-inc", "int main(){int s=0; for(int i=0;i<5;i++){s=s+i;} return s;}"},
		{"for-decl-break", "int main(){int s=0; for(int i=0;;i=i+1){ if(i>=5) break; s=s+i; } return s;}"},
		// break / continue nesting
		{"break-nested", "int main(){int i=0; while(1){ i=i+1; if(i==3) break; } return i;}"},
		{"continue-basic", "int main(){int s=0; for(int i=0;i<10;i=i+1){ if(i<8) continue; s=s+i; } return s;}"},
		{"continue-while", "int main(){int i=0,s=0; while(i<10){i=i+1; if(i%2==0) continue; s=s+1;} return s;}"},
		{"continue-nested", "int main(){int s=0; for(int i=0;i<3;i=i+1){ for(int j=0;j<5;j=j+1){ if(j<3) continue; s=s+1; } } return s;}"},
		{"break-in-switch-in-loop", "int main(){int i=0; while(i<10){ i=i+1; switch(i){ case 3: break; default: continue; } break; } return i;}"},
		// switch
		{"switch-basic", "int main(){int x=2; switch(x){ case 1: return 10; case 2: return 20; case 3: return 30; } return 99;}"},
		{"switch-default", "int main(){int x=9; switch(x){ case 1: return 10; case 2: return 20; default: return 77; } }"},
		{"switch-fallthrough", "int main(){int x=1,s=0; switch(x){ case 1: s=s+1; case 2: s=s+10; break; case 3: s=99; } return s;}"},
		{"switch-fall-into-default", "int main(){int x=2,s=0; switch(x){ case 1: s=1; break; case 2: s=2; default: s=s+10; } return s;}"},
		{"switch-last-nobreak", "int main(){int x=2,s=0; switch(x){ case 1: s=1; break; case 2: s=2; } return s;}"},
		{"switch-nomatch-nodefault", "int main(){int x=5,s=7; switch(x){ case 1: s=1; break; } return s;}"},
		{"switch-char", "int main(){char c=98; switch(c){ case 97: return 1; case 98: return 2; } return 0;}"},
		{"switch-long", "int main(){long x=70000; switch(x){ case 70000: return 1; default: return 2; } }"},
		{"switch-only-default", "int main(){int x=5; switch(x){ default: return 8; } }"},
		{"switch-empty", "int main(){int x=5; switch(x){} return x;}"},
		{"switch-nested", "int main(){int a=1,b=2,r=0; switch(a){ case 1: switch(b){ case 2: r=42; break; default: r=1; } break; default: r=2; } return r;}"},
		{"switch-loop-break", "int main(){int s=0; for(int i=0;i<10;i=i+1){ switch(i){ case 3: break; case 7: break; } s=s+1; } return s;}"},
		// goto / labels (forward and backward)
		{"goto-forward", "int main(){ goto done; return 1; done: return 5; }"},
		{"goto-backward", "int main(){int i=0; again: i=i+1; if(i<5) goto again; return i;}"},
		{"goto-skip", "int main(){int x=1; if(x) goto yes; x=2; yes: return x;}"},
		{"goto-in-loop", "int main(){int i=0; while(i<10){ i=i+1; if(i==3) goto out; } out: return i;}"},
		// return with value
		{"return-expr", "int main(){int a=6,b=7; return a*b-2;}"},
		{"return-long", "int main(){long x=1000000; long y=x*1000; return (y>999999999)?5:6;}"},
		{"return-char-fn", "char f(){ return 65; } int main(){ return f(); }"},
		{"void-noreturn", "void f(){} int main(){f(); return 3;}"},
		// declarations
		{"decl-init", "int main(){int x=5; int y=x+3; return y;}"},
		{"decl-multi", "int main(){int a=1, b=2, c=3; return a+b+c;}"},
		{"decl-noinit", "int main(){int x; x=9; return x;}"},
		{"assign-chain", "int main(){int a,b; a=b=7; return a+b;}"},
		{"compound-assign", "int main(){int x=10; x+=5; x*=2; x-=3; x/=4; x%=5; return x;}"},
		{"pre-inc", "int main(){int i=0; ++i; ++i; return i;}"},
		{"neg-numbers", "int main(){int x=-5; if(x<0) return 1; return 0;}"},
		{"unsigned-char", "int main(){unsigned char c=200; return (c>100)?1:0;}"},
		{"logic-ops", "int main(){int a=1,b=0; if(a&&!b) return 3; return 4;}"},
		{"logic-or", "int main(){int a=0,b=2; if(a||b) return 5; return 6;}"},
		{"ternary", "int main(){int x=4; return (x>2)?100:200;}"},
		{"mod-div", "int main(){int s=0; for(int i=1;i<=10;i=i+1){ if(i%2==0) s=s+i; } return s+17/3;}"},
		// arrays
		{"array-basic", "int main(){int a[5]; a[0]=10; a[4]=20; return a[0]+a[4];}"},
		{"array-init", "int main(){int a[4]={3,4,5,6}; return a[0]+a[3];}"},
		{"array-partial", "int main(){int a[4]={5,6}; int x=a[0]+a[1]; int y=a[2]+a[3]; return x+y;}"},
		{"array-partial-sum", "int main(){int a[4]={5,6}; return a[0]+a[1]+a[2]+a[3];}"},
		{"array-loop", "int main(){int a[10]; for(int i=0;i<10;i=i+1) a[i]=i*2; return a[9];}"},
		{"array-string", "int main(){char s[]=\"hello\"; return s[1];}"},
		{"array-string-sized", "int main(){char s[8]=\"hi\"; return s[0]+s[1]+s[2];}"},
		{"block-scope", "int main(){int x=1; {int y=2; x=x+y;} return x;}"},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "case.c")
			if err := os.WriteFile(src, []byte(tc.src), 0644); err != nil {
				t.Fatal(err)
			}
			seaOut := filepath.Join(dir, "sea_out")
			gccOut := filepath.Join(dir, "gcc_out")
			cmd := exec.Command(seaBin, src, "-o", seaOut, "-single-pass")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("sea compile failed: %v\n%s", err, out)
			}
			cmd = exec.Command("gcc", src, "-o", gccOut)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("gcc compile failed: %v\n%s", err, out)
			}
			rsea := exec.Command(seaOut)
			rsea.Run()
			rcSea := rsea.ProcessState.ExitCode()
			rgcc := exec.Command(gccOut)
			rgcc.Run()
			rcGcc := rgcc.ProcessState.ExitCode()
			if rcSea != rcGcc {
				t.Errorf("exit code mismatch: sea=%d gcc=%d", rcSea, rcGcc)
			}
		})
	}
}
