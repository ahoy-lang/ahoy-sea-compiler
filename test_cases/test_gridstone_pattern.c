#include <stdio.h>
#include <stdlib.h>
#include <stdbool.h>

typedef struct {
    int __arc_refcount;
    char* name;
    int health;
    int attack;
    int range;
    bool can_move;
} CardData;

int main() {
    printf("Test: Exact Gridstone pattern\n");
    
    CardData* result = ({ 
        CardData* __tmp = malloc(sizeof(CardData)); 
        *__tmp = (CardData){
            .__arc_refcount = 1,
            .name = "Necromancer", 
            .health = 3, 
            .attack = 0, 
            .range = 1, 
            .can_move = false
        }; 
        __tmp; 
    });
    
    printf("arc_refcount=%d name=%s health=%d attack=%d range=%d can_move=%d\n",
           result->__arc_refcount, result->name, result->health, result->attack, result->range, 
           result->can_move);
    
    if (result->health == 3 && result->attack == 0 && result->__arc_refcount == 1) {
        printf("SUCCESS!\n");
        return 0;
    } else {
        printf("FAILED! Values are wrong\n");
        return 1;
    }
}
