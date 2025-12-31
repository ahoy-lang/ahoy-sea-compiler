#include <stdio.h>
#include <stdlib.h>

typedef struct {
    int x;
    int y;
} Point;

void test_func(Point* p) {
    printf("In test_func: (%d, %d)\n", p->x, p->y);
}

int main() {
    printf("Test: Direct call with statement expr\n");
    Point* __tmp = malloc(sizeof(Point)); 
    *__tmp = (Point){.x = 30, .y = 40}; 
    printf("After assignment: (%d, %d)\n", __tmp->x, __tmp->y);
    test_func(__tmp);
    
    printf("\nTest: Call with inline statement expr\n");
    test_func(({ Point* __tmp2 = malloc(sizeof(Point)); *__tmp2 = (Point){.x = 50, .y = 60}; __tmp2; }));
    
    return 0;
}
