#include <stdio.h>

typedef struct {
    int x;
    int y;
} Point;

int main() {
    Point p1;
    p1.x = 10;
    p1.y = 20;
    
    int a = p1.x;
    int b = p1.y;
    printf("a=%d b=%d\n", a, b);
    
    return 0;
}
