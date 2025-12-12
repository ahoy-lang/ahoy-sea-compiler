// Comprehensive test of new compiler features added for Gridstone support

// Forward declarations for external functions
extern int fprintf();
extern int printf();

// External symbols
extern void* stderr;

// Test 1: Type modifiers (unsigned, long, etc.)
unsigned int test_unsigned() {
    unsigned int x = 100;
    long y = 1000;
    return x;
}

// Test 2: Anonymous unions
int test_anonymous_union() {
    union { int i; double d; } converter;
    converter.i = 42;
    return converter.i;
}

// Test 3: Named unions with typedef
typedef union {
    int int_val;
    double double_val;
} NumberUnion;

int test_named_union() {
    NumberUnion num;
    num.int_val = 123;
    return num.int_val;
}

// Test 4: Complex statement expressions
typedef struct {
    int* data;
    int length;
} Array;

int test_statement_expression() {
    int result = ({
        int value = 3;
        value;
    });
    
    return result;
}

// Test 5: Standard library symbols (stderr, etc.)
void test_stderr() {
    fprintf(stderr, "Test message to stderr\n");
}

// Test 6: Simple combined test
unsigned long test_combined_features() {
    unsigned long result = 100;
    result += 42;
    return result;
}

int main() {
    printf("Testing compiler features\n");
    return 0;
}
