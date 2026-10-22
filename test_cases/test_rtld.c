// Regression test: <dlfcn.h> is stubbed by the sea preprocessor (system
// headers are deliberately not inlined), so the RTLD_* mode flags must be
// provided as builtins with exact glibc values. bell's Auto-LOD glue code
// passes RTLD_LAZY to dlopen; without the builtin the TU fails with
// "undefined variable: RTLD_LAZY".
#include <dlfcn.h>

int main(void) {
    if (RTLD_LAZY != 1) return 1;
    if (RTLD_NOW != 2) return 2;
    if (RTLD_GLOBAL != 256) return 3;
    if (RTLD_LOCAL != 0) return 4;
    if (RTLD_NOLOAD != 4) return 5;
    if (RTLD_DEEPBIND != 8) return 6;
    if (RTLD_NODELETE != 4096) return 7;
    return 0;
}
