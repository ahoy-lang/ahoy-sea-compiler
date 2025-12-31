#include <stdio.h>

typedef struct {
    int x;
    int y;
} Point;

int main() {
    Point p1;
    p1.x = 10;
    printf("After x assignment: x=%d\n", p1.x);
    p1.y = 20;
    printf("After y assignment: x=%d y=%d\n", p1.x, p1.y);
    return 0;
}
