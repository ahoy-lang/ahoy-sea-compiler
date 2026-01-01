#include <stdio.h>
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
    printf("sizeof(CardData) = %zu\n", sizeof(CardData));
    printf("offsetof arc_refcount = %zu\n", __builtin_offsetof(CardData, __arc_refcount));
    printf("offsetof name = %zu\n", __builtin_offsetof(CardData, name));
    printf("offsetof health = %zu\n", __builtin_offsetof(CardData, health));
    printf("offsetof attack = %zu\n", __builtin_offsetof(CardData, attack));
    printf("offsetof range = %zu\n", __builtin_offsetof(CardData, range));
    printf("offsetof can_move = %zu\n", __builtin_offsetof(CardData, can_move));
    return 0;
}
