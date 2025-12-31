#include <stdio.h>

typedef struct {
    int x;
    int y;
} Point;

int main() {
    Point p1;
    p1.x = 10;
    p1.y = 20;
    printf("x=%d y=%d\n", p1.x, p1.y);
    return 0;
}
