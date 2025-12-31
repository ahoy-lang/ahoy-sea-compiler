#include <stdio.h>

typedef struct {
    int x;
    int y;
} Point;

int main() {
    Point p1;
    p1.x = 10;
    p1.y = 20;
    
    printf("Test with separate declarations:\n");
    int a = p1.x;
    printf("After int a = p1.x: a=%d (expect 10)\n", a);
    
    int b = p1.y;
    printf("After int b = p1.y: b=%d (expect 20)\n", b);
    
    printf("Reading a again: a=%d (expect 10)\n", a);
    printf("Reading b again: b=%d (expect 20)\n", b);
    
    return 0;
}
