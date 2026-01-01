#include <stdio.h>
#include <stdlib.h>
#include <stdbool.h>

typedef struct {
    int __arc_refcount;
    char* name;
    int health;
} CardData;

int main() {
    printf("Test 1: Direct compound literal assignment\n");
    CardData* ptr1 = malloc(sizeof(CardData));
    printf("ptr1 = %p\n", ptr1);
    
    *ptr1 = (CardData){
        .__arc_refcount = 1,
        .name = "Test",
        .health = 99
    };
    
    printf("After assignment: refcount=%d name=%s health=%d\n",
           ptr1->__arc_refcount, ptr1->name, ptr1->health);
    
    return 0;
}
