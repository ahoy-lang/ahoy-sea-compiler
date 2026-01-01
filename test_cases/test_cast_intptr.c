#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <stdbool.h>

typedef struct {
    int __arc_refcount;
    char* name;
    int health;
} CardData;

void test_func(intptr_t value) {
    CardData* card = (CardData*)value;
    printf("In function: card=%p refcount=%d name=%s health=%d\n",
           card, card->__arc_refcount, card->name, card->health);
}

int main() {
    printf("Test: Cast to intptr_t\n");
    
    test_func((intptr_t)({ 
        CardData* __tmp = malloc(sizeof(CardData));
        *__tmp = (CardData){
            .__arc_refcount = 1,
            .name = "Test",
            .health = 99
        };
        __tmp;
    }));
    
    return 0;
}
