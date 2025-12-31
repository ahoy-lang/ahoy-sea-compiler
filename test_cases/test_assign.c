#include <stdio.h>

typedef struct {
    int x;
    int y;
} Point;

int main() {
    Point p1;
    p1.x = 10;
    p1.y = 20;
    
    // Direct use - works
    printf("Direct: p1.x=%d p1.y=%d\n", p1.x, p1.y);
    
    // Assign to variable
    int a;
    a = p1.x;
    printf("After a=p1.x: a=%d\n", a);
    
    int b;
    b = p1.y;
    printf("After b=p1.y: b=%d\n", b);
    
    return 0;
}
