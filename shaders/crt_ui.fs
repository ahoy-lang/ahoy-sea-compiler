#version 330

// Input from vertex shader
in vec2 fragTexCoord;

// Output fragment color
out vec4 finalColor;

// Uniforms
uniform sampler2D texture0;
uniform float time;
uniform vec2 resolution = vec2(2560.0, 1440.0);  // Default resolution

// CRT effect parameters with defaults - NO PIXELATION for UI
uniform float scanlineOpacity = 0.1;
uniform float scanlineWidth = 0.1;
uniform float grillOpacity = 0.0;
uniform float noiseOpacity = 0.005;
uniform float noiseSpeed = 0.1;
uniform float staticNoiseIntensity = 0.05;
uniform float aberration = 0.005;
uniform float brightness = 1.2;
uniform float warpAmount = 0.0;
uniform float vignetteIntensity = 0.1;
uniform float vignetteOpacity = 0.1;
uniform bool discolor = false;
uniform bool pixelate = false;  // NO PIXELATION for UI text

// Random function for noise
vec2 random(vec2 uv) {
    uv = vec2(dot(uv, vec2(127.1, 311.7)), dot(uv, vec2(269.5, 183.3)));
    return -1.0 + 2.0 * fract(sin(uv) * 43758.5453123);
}

// Noise function
float noise(vec2 uv) {
    vec2 uv_index = floor(uv);
    vec2 uv_fract = fract(uv);
    vec2 blur = smoothstep(0.0, 1.0, uv_fract);

    return mix(
        mix(dot(random(uv_index + vec2(0.0, 0.0)), uv_fract - vec2(0.0, 0.0)),
            dot(random(uv_index + vec2(1.0, 0.0)), uv_fract - vec2(1.0, 0.0)), blur.x),
        mix(dot(random(uv_index + vec2(0.0, 1.0)), uv_fract - vec2(0.0, 1.0)),
            dot(random(uv_index + vec2(1.0, 1.0)), uv_fract - vec2(1.0, 1.0)), blur.x),
        blur.y
    ) * 0.5 + 0.5;
}

// Screen warp effect
vec2 warp(vec2 uv) {
    vec2 delta = uv - 0.5;
    float delta2 = dot(delta.xy, delta.xy);
    float delta4 = delta2 * delta2;
    float delta_offset = delta4 * warpAmount;
    return uv + delta * delta_offset;
}

// Vignette effect
float vignette(vec2 uv) {
    uv *= 1.0 - uv.yx;
    float vig = uv.x * uv.y * 15.0;
    return pow(vig, vignetteIntensity * vignetteOpacity);
}

void main() {
    vec2 uv = warp(fragTexCoord);
    vec2 text_uv = uv;

    // Pixelation effect
    if (pixelate) {
        text_uv = ceil(uv * resolution) / resolution;
    }

    // Chromatic aberration
    vec4 color;
    color.r = texture(texture0, text_uv + vec2(aberration * 0.01, 0.0)).r;
    color.g = texture(texture0, text_uv).g;
    color.b = texture(texture0, text_uv - vec2(aberration * 0.01, 0.0)).b;
    color.a = 1.0;

    // Apply grille effect (RGB sub-pixels)
    if (grillOpacity > 0.0) {
        float g_r = smoothstep(0.85, 0.95, abs(sin(uv.x * (resolution.x * 3.14159265))));
        color.r = mix(color.r, color.r * g_r, grillOpacity);

        float g_g = smoothstep(0.85, 0.95, abs(sin(1.05 + uv.x * (resolution.x * 3.14159265))));
        color.g = mix(color.g, color.g * g_g, grillOpacity);

        float g_b = smoothstep(0.85, 0.95, abs(sin(2.1 + uv.x * (resolution.x * 3.14159265))));
        color.b = mix(color.b, color.b * g_b, grillOpacity);
    }

    // Apply brightness
    color.rgb = clamp(color.rgb * brightness, 0.0, 1.0);

    // Scanlines
    float scanlines = 0.5;
    if (scanlineOpacity > 0.0) {
        scanlines = smoothstep(scanlineWidth, scanlineWidth + 0.5, abs(sin(uv.y * (resolution.y * 3.14159265))));
        color.rgb = mix(color.rgb, color.rgb * vec3(scanlines), scanlineOpacity);
    }

    // Noise effect
    if (noiseOpacity > 0.0) {
        float n = smoothstep(0.4, 0.5, noise(uv * vec2(2.0, 200.0) + vec2(10.0, time * noiseSpeed)));
        float roll_line = n * scanlines * clamp(random((ceil(uv * resolution) / resolution) + vec2(time * 0.8, 0.0)).x + 0.8, 0.0, 1.0);
        color.rgb = clamp(mix(color.rgb, color.rgb + roll_line, noiseOpacity), vec3(0.0), vec3(1.0));
    }

    // Static noise
    if (staticNoiseIntensity > 0.0) {
        color.rgb += clamp(random((ceil(uv * resolution) / resolution) + fract(time)).x, 0.0, 1.0) * staticNoiseIntensity;
    }

    // Apply vignette
    color.rgb *= vignette(uv);

    // Desaturation and contrast adjustment
    if (discolor) {
        float saturation = 0.5;
        float contrast = 1.2;
        vec3 greyscale = vec3(color.r + color.g + color.b) / 3.0;
        color.rgb = mix(color.rgb, greyscale, saturation);

        float midpoint = pow(0.5, 2.2);
        color.rgb = (color.rgb - vec3(midpoint)) * contrast + midpoint;
    }

    finalColor = color;
}
