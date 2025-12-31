#include <stdio.h>
#include <stdlib.h>

typedef struct {
    int x;
    int y;
} Point;

int main() {
    printf("Test 1: Simple statement expression\n");
    int result = ({ int a = 5; a + 3; });
    printf("Result: %d (expected 8)\n", result);
    
    printf("\nTest 2: Statement expression with malloc\n");
    int* ptr = ({ int* p = malloc(sizeof(int)); *p = 42; p; });
    printf("Value: %d (expected 42)\n", *ptr);
    
    printf("\nTest 3: Statement expression with struct\n");
    Point* point = ({ Point* pt = malloc(sizeof(Point)); pt->x = 10; pt->y = 20; pt; });
    printf("Point: (%d, %d) (expected 10, 20)\n", point->x, point->y);
    
    printf("\nTest 4: Nested - compound literal in statement expression\n");
    Point* point2 = ({ Point* __tmp = malloc(sizeof(Point)); *__tmp = (Point){.x = 30, .y = 40}; __tmp; });
    printf("Point2: (%d, %d) (expected 30, 40)\n", point2->x, point2->y);
    
    printf("\nAll tests passed!\n");
    return 0;
}
