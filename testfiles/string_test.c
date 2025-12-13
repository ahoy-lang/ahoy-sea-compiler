#include <stdio.h>

void test_strings(const char *s1, const char *s2) {
    printf("s1: %s\n", s1);
    printf("s2: %s\n", s2);
    
    if (s1 && s1[0] != '\0') {
        printf("s1 is not empty\n");
    } else {
        printf("s1 is empty\n");
    }
    
    if (s2 && s2[0] != '\0') {
        printf("s2 is not empty\n");
    } else {
        printf("s2 is empty\n");
    }
}

int main() {
    test_strings("", "shaders/crt.fs");
    test_strings("shaders/wobble.vs", "shaders/wobble.fs");
    return 0;
}
