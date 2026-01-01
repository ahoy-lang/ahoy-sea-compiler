#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <stdbool.h>

typedef enum {
    AHOY_TYPE_INT,
    AHOY_TYPE_STRING,
    AHOY_TYPE_STRUCT
} AhoyValueType;

typedef struct {
    intptr_t* data;
    AhoyValueType* types;
    int length;
    int capacity;
    int is_typed;
    AhoyValueType element_type;
} AhoyArray;

typedef struct {
    int __arc_refcount;
    char* name;
    int health;
    int attack;
    int range;
    bool can_move;
} CardData;

AhoyArray* ahoy_array_push(AhoyArray* arr, intptr_t value, AhoyValueType type) {
    if (arr->length >= arr->capacity) {
        arr->capacity = arr->capacity == 0 ? 4 : arr->capacity * 2;
        arr->data = realloc(arr->data, arr->capacity * sizeof(intptr_t));
        arr->types = realloc(arr->types, arr->capacity * sizeof(AhoyValueType));
    }
    arr->data[arr->length] = value;
    arr->types[arr->length] = type;
    arr->length++;
    return arr;
}

int main() {
    printf("Creating array...\n");
    AhoyArray* card_db = malloc(sizeof(AhoyArray));
    card_db->length = 0;
    card_db->capacity = 0;
    card_db->data = malloc(0);
    card_db->types = malloc(0);
    card_db->is_typed = 1;
    card_db->element_type = AHOY_TYPE_STRUCT;
    
    printf("Pushing first card...\n");
    ahoy_array_push(card_db, (intptr_t)({ 
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
    }), AHOY_TYPE_STRUCT);
    
    printf("SUCCESS! Array has %d elements\n", card_db->length);
    CardData* card = (CardData*)card_db->data[0];
    printf("Card: %s (health=%d)\n", card->name, card->health);
    
    return 0;
}
