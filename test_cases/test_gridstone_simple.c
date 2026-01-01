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
    
    printf("Single field test:\n");
    printf("arc_refcount=%d\n", result->__arc_refcount);
    printf("name=%s\n", result->name);
    printf("health=%d\n", result->health);
    
    return 0;
}
