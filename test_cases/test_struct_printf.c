#include <stdio.h>

typedef struct {
    int x;
    int y;
} Point;

void print_two_ints(int a, int b) {
    printf("a=%d b=%d\n", a, b);
}

int main() {
    Point p1;
    p1.x = 10;
    p1.y = 20;
    
    printf("Test 1 - Direct member access:\n");
    printf("p1.x=%d\n", p1.x);
    printf("p1.y=%d\n", p1.y);
    
    printf("\nTest 2 - Via local variables:\n");
    int a = p1.x;
    int b = p1.y;
    printf("a=%d b=%d\n", a, b);
    
    printf("\nTest 3 - Via function:\n");
    print_two_ints(p1.x, p1.y);
    
    return 0;
}
