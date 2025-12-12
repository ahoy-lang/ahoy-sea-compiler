// Demo of working features added for Gridstone

// Test 1: Type modifiers
unsigned int add_unsigned(unsigned int a, unsigned int b) {
    return a + b;
}

long multiply_long(long a, long b) {
    return a * b;
}

// Test 2: Simple statement expressions  
int calculate() {
    int result = ({
        int x = 10;
        int y = 20;
        x + y;
    });
    return result;
}

// Test 3: Nested statement expressions
int nested_calc() {
    int outer = ({
        int inner = ({
            int val = 5;
            val * 2;
        });
        inner + 10;
    });
    return outer;
}

int main() {
    unsigned int sum = add_unsigned(100, 50);
    long product = multiply_long(10, 20);
    int calc1 = calculate();
    int calc2 = nested_calc();
    
    return sum + (unsigned int)product + calc1 + calc2;
    // Returns: 150 + 200 + 30 + 20 = 400
}
