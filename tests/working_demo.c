// Working demo of new compiler features

// Feature 1: Type modifiers
unsigned int test_unsigned() {
    unsigned int x = 100;
    long y = 200;
    short z = 10;
    return x;
}

// Feature 2: Statement expressions
int test_statement_expr() {
    int result = ({
        int a = 5;
        int b = 10;
        a + b;
    });
    return result;
}

// Feature 3: Long modifier with casts
long test_long_cast() {
    int x = 42;
    long result = (long)x;
    return result * 2;
}

int main() {
    unsigned int val1 = test_unsigned();
    int val2 = test_statement_expr();  
    long val3 = test_long_cast();
    
    return (int)(val1 + val2 + val3);
    // Returns: 100 + 15 + 84 = 199
}
