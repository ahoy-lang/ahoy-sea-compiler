#include <stdio.h>

typedef struct {
    int x;
    int y;
} Point;

int main() {
    Point p1;
    p1.x = 10;
    p1.y = 20;
    
    printf("Reading p1.x twice:\n");
    int a1 = p1.x;
    int a2 = p1.x;
    printf("a1=%d a2=%d\n", a1, a2);
    
    printf("\nReading p1.y twice:\n");
    int b1 = p1.y;
    int b2 = p1.y;
    printf("b1=%d b2=%d\n", b1, b2);
    
    printf("\nReading x then y:\n");
    int c = p1.x;
    int d = p1.y;
    printf("c=%d d=%d\n", c, d);
    
    printf("\nReading y then x:\n");
    int e = p1.y;
    int f = p1.x;
    printf("e=%d f=%d\n", e, f);
    
    return 0;
}
