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
    printf("a=%d\n", a);
    
    int b = p1.y;
    printf("b=%d\n", b);
    
    printf("both: a=%d b=%d\n", a, b);
    
    return 0;
}
