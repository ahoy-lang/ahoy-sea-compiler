/* Regression: values live across a call must not be allocated to
   caller-saved registers (the callee clobbers them; the emitter has no
   save/restore around calls). This is the stbtt rasterizer shape: a float
   accumulator updated by a float-returning helper with five float
   arguments. Before the allocator's call-clobber rule, `s += f(...)`
   compiled the old value of s into %edi, the callee trashed it, and the
   add read garbage (+0.125 instead of -0.875 in the original repro).

   Unit-width trapezoids make every call return exactly `height`, so the
   expected sum is integer-exact. */

static float stbtt__sized_trapezoid_area(float height, float top_width, float bottom_width)
{
   return (top_width + bottom_width) / 2.0f * height;
}

static float stbtt__position_trapezoid_area(float height, float tx0, float tx1, float bx0, float bx1)
{
   return stbtt__sized_trapezoid_area(height, tx1 - tx0, bx1 - bx0);
}

int main(void)
{
    float s = 0.0f;
    /* Extra live values: register pressure so the accumulator competes for
       a register instead of trivially being spilled. */
    float noise1 = 0.5f, noise2 = 1.25f, noise3 = 2.75f;
    int n = 0;

    for (int x = 0; x < 32; x++) {
        float x_top = (float)x * 0.5f;
        float x_bottom = (float)x * 0.5f + noise2;
        s += stbtt__position_trapezoid_area(-1.0f, x_top, x_top + 1.0f,
                                            x_bottom, x_bottom + 1.0f);
        n += x & 3;
        noise3 += noise1;
    }

    /* Every trapezoid is exactly -1.0 wide. */
    if (s != -32.0f) return 1;
    if (n != 48) return 2;
    if (noise3 != 2.75f + 32.0f * 0.5f) return 3;
    return 0;
}
