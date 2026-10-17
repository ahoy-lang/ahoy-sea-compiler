/* Regression: local struct-array brace initializer. `stbtt__edge a[6] =
   {{...},...}` used to leave every element zeroed: the parser resolved the
   first nested brace against struct member 0 (a float), found no nested
   struct, and silently skipped the braces. Also covers struct element
   copy (e[j] = e[j+1]) for structs over 8 bytes. */

typedef struct { float x0, y0, x1, y1; int invert; } stbtt__edge;

static void bubblesort(stbtt__edge *e, int n)
{
    for (int i = 0; i < n - 1; i++)
        for (int j = 0; j < n - 1 - i; j++)
            if (e[j].y0 > e[j + 1].y0) {
                stbtt__edge t = e[j];
                e[j] = e[j + 1];
                e[j + 1] = t;
            }
}

int main(void)
{
    stbtt__edge a[6] = {
        { 0, 12.0f, 0, 20.0f, 0 },
        { 0,  5.0f, 0, 10.0f, 1 },
        { 0, 12.0f, 0, 15.0f, 1 },
        { 0,  5.0f, 0,  8.0f, 0 },
        { 0, 12.0f, 0, 25.0f, 0 },
        { 0,  3.0f, 0,  9.0f, 1 },
    };

    bubblesort(a, 6);

    /* Stable sort by y0: 3, 5, 5, 12, 12, 12. */
    if (a[0].y0 != 3.0f || a[0].y1 != 9.0f || a[0].invert != 1) return 1;
    if (a[1].y0 != 5.0f || a[1].y1 != 10.0f || a[1].invert != 1) return 2;
    if (a[2].y0 != 5.0f || a[2].y1 != 8.0f || a[2].invert != 0) return 3;
    if (a[3].y0 != 12.0f || a[3].y1 != 20.0f || a[3].invert != 0) return 4;
    if (a[4].y0 != 12.0f || a[4].y1 != 15.0f || a[4].invert != 1) return 5;
    if (a[5].y0 != 12.0f || a[5].y1 != 25.0f || a[5].invert != 0) return 6;
    return 0;
}
