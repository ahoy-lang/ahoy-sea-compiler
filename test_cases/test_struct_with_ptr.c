#include <stdio.h>

typedef struct {
    int* data;
    int length;
} Array;

int main() {
    Array arr;
    int x = 42;
    arr.data = &x;
    arr.length = 1;
    printf("arr.data[0] = %d, arr.length = %d\n", arr.data[0], arr.length);
    return 0;
}
