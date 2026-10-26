#version 450

layout(location = 0) in vec4 inColor;
layout(location = 1) in vec2 inLocal;
layout(location = 2) in vec2 inSize;
layout(location = 3) in float inRadius;
layout(location = 4) in vec2 inFragment;
layout(location = 5) in vec2 inTriangleA;
layout(location = 6) in vec2 inTriangleB;
layout(location = 7) in vec2 inTriangleC;
layout(location = 8) flat in uint inTriangleMask;

layout(location = 0) out vec4 outColor;

float edgeDistance(vec2 point, vec2 from, vec2 to) {
    vec2 edge = to - from;
    float edgeLength = max(length(edge), 1e-6);
    return (edge.x * (point.y - from.y) - edge.y * (point.x - from.x)) / edgeLength;
}
float segmentDistance(vec2 p, vec2 a, vec2 b) {
    vec2 edge = b - a;
    float along = clamp(dot(p - a, edge) / max(dot(edge, edge), 1e-12), 0.0, 1.0);
    return length(p - a - along * edge);
}

// Logical pixels per device pixel at this fragment. Edges feathered over a
// fixed logical band smear across several device pixels on a scaled display;
// one device pixel keeps thin strokes and corners crisp at any scale.
float devicePixel(vec2 position) {
    return max(fwidth(position).x, 1e-4);
}

// Signed distance from p to a rounded box centred on the origin.
float roundedBoxDistance(vec2 p, vec2 halfSize, float radius) {
    vec2 q = abs(p) - halfSize + radius;
    return length(max(q, vec2(0.0))) + min(max(q.x, q.y), 0.0) - radius;
}

// A soft shadow. The shadow's shape is the casting rectangle moved by the offset
// and grown by the spread (the corner radius grows with it, a sharp corner
// staying sharp). Its edge is spread over a band of blur on either side: a
// smoothstep of the signed distance, which follows a Gaussian of deviation
// blur / 2 (the CSS box-shadow blur) to within a few percent. The casting
// rectangle itself is cut out, so a translucent surface drawn over the shadow is
// not darkened by it.
vec4 shadowColor(float pixel) {
    vec2 offset = inTriangleA;
    float spread = inTriangleB.x;
    float blur = inTriangleB.y;
    vec2 halfSize = max(inSize * 0.5 + spread, vec2(0.0));
    float radius = inRadius > 0.0 ? clamp(inRadius + spread, 0.0, min(halfSize.x, halfSize.y)) : 0.0;
    float distance = roundedBoxDistance(inLocal - inSize * 0.5 - offset, halfSize, radius);
    float t = clamp(0.5 - distance / (2.0 * blur), 0.0, 1.0);
    float alpha = t * t * (3.0 - 2.0 * t);
    float hole = min(inRadius, min(inSize.x, inSize.y) * 0.5);
    float inside = roundedBoxDistance(inLocal - inSize * 0.5, inSize * 0.5, hole);
    alpha *= 1.0 - clamp(0.5 - inside / pixel, 0.0, 1.0);
    alpha *= inColor.a;
    return vec4(inColor.rgb * alpha, alpha);
}

void main() {
    if (inTriangleMask > 0) {
        float area = (inTriangleB.x - inTriangleA.x) * (inTriangleC.y - inTriangleA.y) -
            (inTriangleB.y - inTriangleA.y) * (inTriangleC.x - inTriangleA.x);
        float orientation = area > 0.0 ? 1.0 : -1.0;
        uint edgeMask = inTriangleMask - 1;
        float distance0 = (edgeMask & 1u) != 0u ? orientation * edgeDistance(inFragment, inTriangleA, inTriangleB) : 1e6;
        float distance1 = (edgeMask & 2u) != 0u ? orientation * edgeDistance(inFragment, inTriangleB, inTriangleC) : 1e6;
        float distance2 = (edgeMask & 4u) != 0u ? orientation * edgeDistance(inFragment, inTriangleC, inTriangleA) : 1e6;
        float inside = min(distance0, min(distance1, distance2));
        float pixel = devicePixel(inFragment);
        if (inside < 0.0) {
            // Acute offset-line miters must not paint beyond the actual ends.
            float s0 = (edgeMask & 1u) != 0u ? segmentDistance(inFragment, inTriangleA, inTriangleB) : 1e6;
            float s1 = (edgeMask & 2u) != 0u ? segmentDistance(inFragment, inTriangleB, inTriangleC) : 1e6;
            float s2 = (edgeMask & 4u) != 0u ? segmentDistance(inFragment, inTriangleC, inTriangleA) : 1e6;
            inside = -min(s0, min(s1, s2));
        }
        float coverage = edgeMask == 0 ? 1.0 : clamp(0.5 + inside / pixel, 0.0, 1.0);
        vec4 shade = clamp(inColor, 0.0, 1.0);
        float alpha = shade.a * coverage;
        outColor = vec4(shade.rgb * alpha, alpha);
        return;
    }
    if (inTriangleB.y > 0.0) {
        outColor = shadowColor(devicePixel(inLocal));
        return;
    }
    float radius = min(inRadius, min(inSize.x, inSize.y) * 0.5);
    float coverage = 1.0;
    if (radius > 0.0) {
        vec2 distanceVector = abs(inLocal - inSize * 0.5) - inSize * 0.5 + radius;
        float distance = length(max(distanceVector, vec2(0.0))) + min(max(distanceVector.x, distanceVector.y), 0.0) - radius;
        coverage = clamp(0.5 - distance / devicePixel(inLocal), 0.0, 1.0);
    }
    float alpha = inColor.a * coverage;
    outColor = vec4(inColor.rgb * alpha, alpha);
}
