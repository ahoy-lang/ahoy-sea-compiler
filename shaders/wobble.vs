#version 330

// Input vertex attributes
in vec3 vertexPosition;
in vec2 vertexTexCoord;
in vec3 vertexNormal;
in vec4 vertexColor;

// Input uniform values
uniform mat4 mvp;
uniform float time;

// Shader parameters
uniform float interval = 0.1;
uniform float timeDelay = 0.2;
uniform float intensityX = 0.15;
uniform float intensityY = 0.05;
uniform float seed = 42.0;

// Output vertex attributes (to fragment shader)
out vec2 fragTexCoord;
out vec4 fragColor;

void main()
{
    float chunk = floor((time + timeDelay) / interval);
    float seedNum = vertexPosition.x + vertexPosition.y + chunk + seed;
    float offsetX = sin(seedNum * 12.9898) * 43758.5453;
    float offsetY = sin(seedNum * 32.9472) * 94726.0482;
    offsetX = fract(offsetX);
    offsetX = offsetX * 2.0 - 1.0;
    offsetY = fract(offsetY);
    offsetY = offsetY * 2.0 - 1.0;

    vec3 wobbledPosition = vertexPosition + vec3(offsetX * intensityX, offsetY * intensityY, 0.0);

    // Send vertex attributes to fragment shader
    fragTexCoord = vertexTexCoord;
    fragColor = vertexColor;

    // Calculate final vertex position
    gl_Position = mvp * vec4(wobbledPosition, 1.0);
}
