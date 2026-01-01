#include <stdio.h>
#include <stdlib.h>
#include <stdbool.h>

typedef struct {
    int __arc_refcount;
    char* name;
    int health;
} CardData;

int main() {
    printf("Test: Compound literal in statement expression\n");
    
    CardData* result = ({ 
        CardData* __tmp = malloc(sizeof(CardData));
        printf("Inside statement expr: __tmp = %p\n", __tmp);
        *__tmp = (CardData){
            .__arc_refcount = 1,
            .name = "Test",
            .health = 99
        };
        printf("After compound literal assignment\n");
        __tmp;
    });
    
    printf("result = %p\n", result);
    printf("refcount=%d name=%s health=%d\n",
           result->__arc_refcount, result->name, result->health);
    
    return 0;
}
