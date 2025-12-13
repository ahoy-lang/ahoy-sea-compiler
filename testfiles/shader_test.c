#include <stdio.h>

// Mock raylib types
typedef struct Shader {
    unsigned int id;
    int *locs;
} Shader;

// Mock LoadShader function that tests string parameter passing
Shader LoadShader(const char *vsFileName, const char *fsFileName) {
    printf("INFO: FILEIO: [] Failed to open text file\n");
    
    if (vsFileName && vsFileName[0] != '\0') {
        printf("INFO: FILEIO: [%s] Text file loaded successfully\n", vsFileName);
    }
    
    if (fsFileName && fsFileName[0] != '\0') {
        printf("INFO: FILEIO: [%s] Text file loaded successfully\n", fsFileName);
    } else {
        printf("WARNING: FILEIO: [%s] Failed to open text file\n", fsFileName ? fsFileName : "(null)");
    }
    
    Shader shader;
    shader.id = 5;
    shader.locs = (int *)0;
    return shader;
}

int main() {
    Shader crtShader = LoadShader("", "shaders/crt.fs");
    Shader crtUIShader = LoadShader("", "shaders/crt_ui.fs");
    Shader wobbleShader = LoadShader("shaders/wobble.vs", "shaders/wobble.fs");
    
    printf("Shaders loaded: %u, %u, %u\n", crtShader.id, crtUIShader.id, wobbleShader.id);
    
    return 0;
}
