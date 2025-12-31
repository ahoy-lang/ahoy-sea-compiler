#include <stdio.h>

typedef struct {
    int x;
    int y;
} Point;

int main() {
    Point p1;
    p1.x = 10;
    printf("p1.x address: %p, value: %d\n", &p1.x, p1.x);
    p1.y = 20;
    printf("p1.y address: %p, value: %d\n", &p1.y, p1.y);
    printf("Offset between x and y: %ld\n", (char*)&p1.y - (char*)&p1.x);
    return 0;
}
