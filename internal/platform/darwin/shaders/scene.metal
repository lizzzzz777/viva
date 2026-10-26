#include <metal_stdlib>

using namespace metal;

struct RectangleInstance {
    float4 bounds;
    float4 color;
    float4 style;
    // A triangle's colors at its second and third corner; color is the first's.
    float4 shade1;
    float4 shade2;
};

struct VertexOutput {
    float4 position [[position]];
    float4 color;
    float2 localPosition;
    float2 size;
    float radius;
    float2 fragPixel;
    float2 triA;
    float2 triB;
    float2 triC;
    uint triangleMask [[flat]];
};

static float cross2(float2 a, float2 b) { return a.x*b.y - a.y*b.x; }
static float segmentDistance(float2 p, float2 a, float2 b) {
    float2 edge = b - a;
    float along = saturate(dot(p - a, edge) / max(dot(edge, edge), 1e-12));
    return length(p - a - along * edge);
}
// Expand silhouette edges only; shared edges keep their supporting line.
static float2 fringeVertex(float2 previous, float2 position, float2 next, uint previousEdge, uint nextEdge, uint mask, float pixelSize) {
    float2 before = position - previous, after = next - position;
    float area = cross2(before, after);
    if (abs(area) < 1e-8 || mask == 0u) return position;
    float orientation = area > 0.0 ? 1.0 : -1.0;
    float h = 0.5 * pixelSize * orientation;
    float first = (mask & previousEdge) != 0u ? -h * length(before) : 0.0;
    float second = (mask & nextEdge) != 0u ? -h * length(after) : 0.0;
    return position + (first * after - second * before) / area;
}

vertex VertexOutput viva_rectangle_vertex(
    uint vertexID [[vertex_id]],
    uint instanceID [[instance_id]],
    const device RectangleInstance *instances [[buffer(0)]],
    constant float4 &viewportSize [[buffer(1)]]) {
    constexpr float2 corners[6] = {
        float2(0.0, 0.0),
        float2(1.0, 0.0),
        float2(0.0, 1.0),
        float2(0.0, 1.0),
        float2(1.0, 0.0),
        float2(1.0, 1.0),
    };

    RectangleInstance instance = instances[instanceID];
    bool isTriangle = instance.style.w > 0.5;
    float2 pixel;
    float2 localPixel;
    float2 size = instance.bounds.zw;
    float radius = instance.style.x;
    float2 triA = 0.0;
    float2 triB = 0.0;
    float2 triC = 0.0;
    uint triangleMask = 0;
    float4 color = instance.color;
    if (isTriangle) {
        // Triangle slots: bounds=(p0, p1),
        // style=(p2.x, layer, p2.y, edgeMask+1), and the corner colors
        // color, shade1, shade2. The rasterizer interpolates them linearly
        // across the face, so a flat triangle (all three equal) is unchanged.
        // The quad's second triangle collapses onto p0 and rasterizes nothing.
        const float2 points[3] = {
            instance.bounds.xy,
            instance.bounds.zw,
            instance.style.xz,
        };
        uint index = vertexID < 3 ? vertexID : 0u;
        uint edgeMask = uint(instance.style.w + 0.5) - 1u;
        uint previous = (index + 2u) % 3u;
        pixel = fringeVertex(points[previous], points[index], points[(index + 1u) % 3u], 1u << previous, 1u << index, edgeMask, viewportSize.z);
        // Extrapolate corner colors so interpolation stays on the original mesh.
        float2 ab = points[1] - points[0], ac = points[2] - points[0];
        float area = cross2(ab, ac);
        if (abs(area) >= 1e-8) {
            float2 delta = pixel - points[0];
            color = instance.color + cross2(delta, ac) / area * (instance.shade1 - instance.color)
                + cross2(ab, delta) / area * (instance.shade2 - instance.color);
        }
        localPixel = 0.0;
        size = 0.0;
        radius = 0.0;
        triA = points[0];
        triB = points[1];
        triC = points[2];
        triangleMask = uint(instance.style.w + 0.5);
    } else if (instance.style.z > 0.0) {
        // Shadow slots: bounds and style.x are the rounded rectangle that casts
        // the shadow, style.z its blur, shade1 = (offset.x, offset.y, spread). The
        // quad grows by the most the shadow reaches; local stays measured from the
        // casting rectangle's corner, so it runs negative on the quad's near side.
        // The offset, spread and blur travel to the fragment in triA and triB,
        // which a rectangle has no use for.
        float extent = instance.style.z + max(instance.shade1.z, 0.0) +
            max(max(instance.shade1.x, -instance.shade1.x), max(instance.shade1.y, -instance.shade1.y));
        pixel = instance.bounds.xy - extent + corners[vertexID] * (instance.bounds.zw + 2.0 * extent);
        localPixel = pixel - instance.bounds.xy;
        triA = instance.shade1.xy;
        triB = float2(instance.shade1.z, instance.style.z);
    } else {
        pixel = instance.bounds.xy + corners[vertexID] * instance.bounds.zw;
        localPixel = corners[vertexID] * instance.bounds.zw;
    }
    float2 normalized = float2(
        pixel.x / viewportSize.x * 2.0 - 1.0,
        1.0 - pixel.y / viewportSize.y * 2.0
    );

    VertexOutput output;
    output.position = float4(normalized, 0.0, 1.0);
    output.color = color;
    output.localPosition = localPixel;
    output.size = size;
    output.radius = radius;
    output.fragPixel = pixel;
    output.triA = triA;
    output.triB = triB;
    output.triC = triC;
    output.triangleMask = triangleMask;
    return output;
}

// Signed pixel distance from p to the directed line a→b.
static float edgeDistance(float2 p, float2 a, float2 b) {
    float2 edge = b - a;
    float length = max(sqrt(edge.x * edge.x + edge.y * edge.y), 1e-6);
    return (edge.x * (p.y - a.y) - edge.y * (p.x - a.x)) / length;
}

// Logical pixels per device pixel at this fragment. Coordinates here are
// logical, so an edge feathered over a fixed logical band is that many device
// pixels wide on a scaled display: 1.5 logical pixels is a 3-pixel smear at 2x.
// Antialiasing one device pixel wide keeps thin strokes and corners crisp at
// any scale.
static float devicePixel(float2 position) {
    return max(fwidth(position).x, 1e-4);
}

// Signed distance from p to a rounded box centred on the origin.
static float roundedBoxDistance(float2 p, float2 halfSize, float radius) {
    float2 q = abs(p) - halfSize + radius;
    return length(max(q, 0.0)) + min(max(q.x, q.y), 0.0) - radius;
}

// A soft shadow. The shadow's shape is the casting rectangle moved by the
// offset and grown by the spread (the corner radius grows with it, a sharp
// corner staying sharp). Its edge is spread over a band of blur on either side:
// a smoothstep of the signed distance, which follows a Gaussian of deviation
// blur / 2 (the CSS box-shadow blur) to within a few percent. The casting
// rectangle itself is cut out, so a translucent surface drawn over the shadow is
// not darkened by it.
static float4 shadowColor(VertexOutput input) {
    float2 offset = input.triA;
    float spread = input.triB.x;
    float blur = input.triB.y;
    float2 halfSize = max(input.size * 0.5 + spread, 0.0);
    float radius = input.radius > 0.0 ? clamp(input.radius + spread, 0.0, min(halfSize.x, halfSize.y)) : 0.0;
    float distance = roundedBoxDistance(input.localPosition - input.size * 0.5 - offset, halfSize, radius);
    float t = saturate(0.5 - distance / (2.0 * blur));
    float alpha = t * t * (3.0 - 2.0 * t);
    float hole = min(input.radius, min(input.size.x, input.size.y) * 0.5);
    float inside = roundedBoxDistance(input.localPosition - input.size * 0.5, input.size * 0.5, hole);
    alpha *= 1.0 - saturate(0.5 - inside / devicePixel(input.localPosition));
    return float4(input.color.rgb, input.color.a * alpha);
}

fragment float4 viva_rectangle_fragment(VertexOutput input [[stage_in]]) {
    if (input.triangleMask > 0) {
        // Winding-independent coverage: the signed distance to the nearest
        // outer mesh edge is feathered over one device pixel, the same band
        // the rounded-rect corners use. Shared tessellation edges stay opaque.
        float area2 = (input.triB.x - input.triA.x) * (input.triC.y - input.triA.y) -
            (input.triB.y - input.triA.y) * (input.triC.x - input.triA.x);
        float sign = area2 > 0.0 ? 1.0 : -1.0;
        uint edgeMask = input.triangleMask - 1;
        float d0 = (edgeMask & 1u) != 0u ? sign * edgeDistance(input.fragPixel, input.triA, input.triB) : 1e6;
        float d1 = (edgeMask & 2u) != 0u ? sign * edgeDistance(input.fragPixel, input.triB, input.triC) : 1e6;
        float d2 = (edgeMask & 4u) != 0u ? sign * edgeDistance(input.fragPixel, input.triC, input.triA) : 1e6;
        float inside = min(d0, min(d1, d2));
        float pixel = devicePixel(input.fragPixel);
        if (inside < 0.0) {
            // Acute offset-line miters must not paint beyond the actual ends.
            float s0 = (edgeMask & 1u) != 0u ? segmentDistance(input.fragPixel, input.triA, input.triB) : 1e6;
            float s1 = (edgeMask & 2u) != 0u ? segmentDistance(input.fragPixel, input.triB, input.triC) : 1e6;
            float s2 = (edgeMask & 4u) != 0u ? segmentDistance(input.fragPixel, input.triC, input.triA) : 1e6;
            inside = -min(s0, min(s1, s2));
        }
        float coverage = edgeMask == 0 ? 1.0 : saturate(0.5 + inside / pixel);
        float4 shade = saturate(input.color);
        return float4(shade.rgb, shade.a * coverage);
    }
    if (input.triB.y > 0.0) {
        return shadowColor(input);
    }
    float radius = min(input.radius, min(input.size.x, input.size.y) * 0.5);
    if (radius <= 0.0) {
        return input.color;
    }
    float2 centered = input.localPosition - input.size * 0.5;
    float2 inner = input.size * 0.5 - radius;
    float2 distanceVector = abs(centered) - inner;
    float distance = length(max(distanceVector, 0.0)) + min(max(distanceVector.x, distanceVector.y), 0.0) - radius;
    float coverage = saturate(0.5 - distance / devicePixel(input.localPosition));
    return float4(input.color.rgb, input.color.a * coverage);
}

struct TextVertexOutput {
    float4 position [[position]];
    float2 textureCoordinate;
};

vertex TextVertexOutput viva_text_vertex(
    uint vertexID [[vertex_id]],
    constant float4 &bounds [[buffer(0)]],
    constant float2 &viewportSize [[buffer(1)]]) {
    constexpr float2 corners[6] = {
        float2(0.0, 0.0),
        float2(1.0, 0.0),
        float2(0.0, 1.0),
        float2(0.0, 1.0),
        float2(1.0, 0.0),
        float2(1.0, 1.0),
    };

    float2 corner = corners[vertexID];
    float2 pixel = bounds.xy + corner * bounds.zw;
    float2 normalized = float2(
        pixel.x / viewportSize.x * 2.0 - 1.0,
        1.0 - pixel.y / viewportSize.y * 2.0
    );

    TextVertexOutput output;
    output.position = float4(normalized, 0.0, 1.0);
    output.textureCoordinate = corner;
    return output;
}

fragment float4 viva_text_fragment(
    TextVertexOutput input [[stage_in]],
    texture2d<float> texture [[texture(0)]]) {
    constexpr sampler textureSampler(
        mag_filter::linear,
        min_filter::linear,
        address::clamp_to_edge
    );
    return texture.sample(textureSampler, input.textureCoordinate);
}
