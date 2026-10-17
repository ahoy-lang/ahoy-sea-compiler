/* Regression: float call-argument staging temps must not be allocated into
 * argument registers (%rdi/%rsi/...) that already hold earlier integer
 * arguments. The trailing float args materialize through temps; if such a
 * temp is colored %rdi/%rsi it clobbers a pointer/int arg that was set up
 * just before (stbtt_Rasterize received 1.0f's bit pattern as its vertices
 * pointer this way). See register_allocator.go collectArgRegBusy.
 * Exit code 0 = all arguments arrived intact. */
#include <stdio.h>
#include <stdint.h>

static uintptr_t expect_ptr;
static float expect_b, expect_c, expect_d;
static int errors = 0;

__attribute__((noinline))
static void mix(int *p, float a, int n, float b, float c, float d) {
    uintptr_t got = (uintptr_t)p;
    if (got != expect_ptr) {
        errors++;
        printf("p: got %lu want %lu\n", (unsigned long)got, (unsigned long)expect_ptr);
    }
    if (a != 0.25f) { errors++; printf("a wrong\n"); }
    if (n != 42) { errors++; printf("n wrong\n"); }
    if (b != expect_b) { errors++; printf("b wrong\n"); }
    if (c != expect_c) { errors++; printf("c wrong\n"); }
    if (d != expect_d) { errors++; printf("d wrong\n"); }
}

int main(void) {
    int x = 7;
    float b = 1.5f, c = 2.5f, d = 3.5f;
    expect_ptr = (uintptr_t)&x;
    expect_b = b; expect_c = c; expect_d = d;
    mix(&x, 0.25f, 42, b, c, d);          /* floats loaded from memory */
    mix(&x, 0.25f, 42, 1.5f, 2.5f, 3.5f); /* floats as immediates */
    if (x != 7) { errors++; printf("x clobbered\n"); }
    return errors;
}
