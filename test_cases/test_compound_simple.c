#include <stdio.h>
#include <stdlib.h>

typedef struct {
    int x;
    int y;
} Point;

int main() {
    printf("Test 1: Create and init Point\n");
    Point p1 = {.x = 10, .y = 20};
    printf("p1: (%d, %d)\n", p1.x, p1.y);
    
    printf("\nTest 2: Malloc and assign via pointer\n");
    Point* p2 = malloc(sizeof(Point));
    p2->x = 30;
    p2->y = 40;
    printf("p2: (%d, %d)\n", p2->x, p2->y);
    
    printf("\nTest 3: Compound literal on stack\n");
    Point p3 = (Point){.x = 50, .y = 60};
    printf("p3: (%d, %d)\n", p3.x, p3.y);
    
    printf("\nTest 4: Malloc pointer and dereference assign compound literal\n");
    Point* p4 = malloc(sizeof(Point));
    printf("p4 allocated at: %p\n", p4);
    *p4 = (Point){.x = 70, .y = 80};
    printf("p4: (%d, %d)\n", p4->x, p4->y);
    
    return 0;
}
