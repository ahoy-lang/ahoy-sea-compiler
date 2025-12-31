#include <stdio.h>
#include <stdlib.h>
#include <stdint.h>
#include <stdbool.h>

typedef enum {
    AHOY_TYPE_INT,
    AHOY_TYPE_FLOAT,
    AHOY_TYPE_STRING,
    AHOY_TYPE_STRUCT,
    AHOY_TYPE_BOOL,
    AHOY_TYPE_NULL
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
    const char* name;
    int health;
    int attack;
    int range;
    bool can_move;
    int __arc_refcount;
} CardData;

AhoyArray* ahoy_array_push(AhoyArray* arr, intptr_t value, AhoyValueType type) {
    printf("ahoy_array_push called: arr=%p, value=%ld, type=%d\n", arr, value, type);
    printf("  Before: length=%d, capacity=%d, data=%p, types=%p\n", 
           arr->length, arr->capacity, arr->data, arr->types);
    
    if (arr->length >= arr->capacity) {
        arr->capacity = arr->capacity == 0 ? 4 : arr->capacity * 2;
        printf("  Reallocating to capacity=%d\n", arr->capacity);
        arr->data = realloc(arr->data, arr->capacity * sizeof(intptr_t));
        arr->types = realloc(arr->types, arr->capacity * sizeof(AhoyValueType));
        printf("  After realloc: data=%p, types=%p\n", arr->data, arr->types);
    }
    
    arr->data[arr->length] = value;
    arr->types[arr->length] = type;
    arr->length++;
    
    printf("  After: length=%d\n", arr->length);
    return arr;
}

int main() {
    printf("Creating initial array...\n");
    AhoyArray* card_db = ({ 
        AhoyArray* arr_0 = malloc(sizeof(AhoyArray)); 
        arr_0->length = 0; 
        arr_0->capacity = 0; 
        arr_0->data = malloc(0 * sizeof(intptr_t)); 
        arr_0->types = malloc(0 * sizeof(AhoyValueType)); 
        arr_0->is_typed = 1; 
        arr_0->element_type = AHOY_TYPE_STRUCT; 
        arr_0; 
    });
    
    printf("\nInitial array created: %p\n", card_db);
    printf("  length=%d, capacity=%d, data=%p, types=%p\n",
           card_db->length, card_db->capacity, card_db->data, card_db->types);
    
    printf("\nPushing first card...\n");
    ahoy_array_push(card_db, (intptr_t)({ 
        CardData* __tmp = malloc(sizeof(CardData)); 
        *__tmp = (CardData){
            .name = "Necromancer", 
            .health = 3, 
            .attack = 0, 
            .range = 1, 
            .can_move = false, 
            .__arc_refcount = 1
        }; 
        __tmp; 
    }), AHOY_TYPE_STRUCT);
    
    printf("\nPushing second card...\n");
    ahoy_array_push(card_db, (intptr_t)({ 
        CardData* __tmp = malloc(sizeof(CardData)); 
        *__tmp = (CardData){
            .name = "Skeleton", 
            .health = 1, 
            .attack = 1, 
            .range = 1, 
            .can_move = false, 
            .__arc_refcount = 1
        }; 
        __tmp; 
    }), AHOY_TYPE_STRUCT);
    
    printf("\nSuccess! Array has %d elements\n", card_db->length);
    return 0;
}
