#include <math.h>
#include <stdint.h>
#include <stdio.h>
#include <string.h>
#include <sys/stat.h>
#include <vips/vips.h>

#define MAX_BYTES 100000000
#define MAX_PIXELS 40000000
#define INPUT "/work/input"
#define OUTPUT "/work/output"

enum format { KEEP, JPEG, PNG, WEBP };

static uint32_t dimension(const unsigned char *p) {
    return ((uint32_t) p[0] << 24) | ((uint32_t) p[1] << 16) |
        ((uint32_t) p[2] << 8) | p[3];
}

static int bounded_dimensions(int width, int height) {
    return width > 0 && height > 0 && (int64_t) width * height <= MAX_PIXELS;
}

static int copy_stream(FILE *in, FILE *out, size_t limit) {
    unsigned char buf[65536];
    size_t total = 0;
    size_t n;
    while ((n = fread(buf, 1, sizeof(buf), in)) != 0) {
        if (n > limit - total || fwrite(buf, 1, n, out) != n) return -1;
        total += n;
    }
    return total && !ferror(in) ? 0 : -1;
}

static int load(VipsImage **image, enum format *format) {
    const char *loader = vips_foreign_find_load(INPUT);
    if (!loader) return -1;
    if (!strcmp(loader, "VipsForeignLoadJpegFile")) {
        *format = JPEG;
        return vips_jpegload(INPUT, image, "fail_on", VIPS_FAIL_ON_WARNING, NULL);
    }
    if (!strcmp(loader, "VipsForeignLoadPngFile")) {
        *format = PNG;
        return vips_pngload(INPUT, image, "fail_on", VIPS_FAIL_ON_WARNING, NULL);
    }
    if (!strcmp(loader, "VipsForeignLoadWebpFile")) {
        *format = WEBP;
        return vips_webpload(INPUT, image, "fail_on", VIPS_FAIL_ON_WARNING, NULL);
    }
    return -1;
}

static int save(VipsImage *image, enum format target, int quality, int lossless) {
    if (target == JPEG && !lossless)
        return vips_jpegsave(image, OUTPUT, "Q", quality, "optimize_coding", TRUE, "keep", VIPS_FOREIGN_KEEP_NONE, NULL);
    if (target == PNG)
        return vips_pngsave(image, OUTPUT, "compression", 9, "keep", VIPS_FOREIGN_KEEP_NONE, NULL);
    if (target == WEBP)
        return vips_webpsave(image, OUTPUT, "Q", quality, "lossless", lossless,
            "effort", 4, "keep", VIPS_FOREIGN_KEEP_NONE, NULL);
    return -1;
}

static int transform(const unsigned char *header) {
    VipsImage *source = NULL, *oriented = NULL, *colour = NULL;
    VipsImage *premultiplied = NULL, *scaled = NULL, *unpremultiplied = NULL, *flat = NULL;
    VipsArrayDouble *background = NULL;
    enum format original = KEEP;
    enum format target = header[4];
    int quality = header[5], flags = header[6];
    int status = 20;
    uint32_t max_w = dimension(header + 10), max_h = dimension(header + 14);

    int svg = !memcmp(header, "ALS1", 4);
    if ((!svg && memcmp(header, "ALI1", 4)) || target > WEBP ||
        (svg && ((target != PNG && target != JPEG) || flags)) ||
        quality < 1 || quality > 100 || (flags & ~15) ||
        ((flags & 8) && (!max_w || !max_h || max_w > MAX_PIXELS || max_h > MAX_PIXELS))) goto done;

    if ((svg ? vips_svgload(INPUT, &source, "unlimited", FALSE, NULL) : load(&source, &original)) ||
        !bounded_dimensions(vips_image_get_width(source), vips_image_get_height(source)) ||
        vips_image_get_n_pages(source) != 1 ||
        vips_autorot(source, &oriented, NULL)) goto done;

    if (target == KEEP) target = original;
    if (vips_image_get_typeof(oriented, VIPS_META_ICC_NAME)) {
        const void *profile = NULL;
        size_t length = 0;
        if (vips_image_get_blob(oriented, VIPS_META_ICC_NAME, &profile, &length) ||
            length > 4 * 1024 * 1024 || !vips_icc_is_compatible_profile(oriented, profile, length) ||
            vips_icc_transform(oriented, &colour, "srgb", "embedded", TRUE, "depth", 8, NULL)) goto done;
    } else if (vips_colourspace(oriented, &colour, VIPS_INTERPRETATION_sRGB, NULL)) goto done;

    VipsImage *image = colour;
    if (flags & 8) {
        int width = vips_image_get_width(image), height = vips_image_get_height(image);
        double sx = (double) max_w / width, sy = (double) max_h / height;
        if (flags & 2) sx = sy = fmin(sx, sy);
        if (!(flags & 4)) { sx = fmin(1, sx); sy = fmin(1, sy); }
        int out_w = (int) fmax(1, round(width * sx));
        int out_h = (int) fmax(1, round(height * sy));
        if (!bounded_dimensions(out_w, out_h)) goto done;
        if (width != out_w || height != out_h) {
            int alpha = vips_image_hasalpha(image);
            if (alpha) {
                if (vips_premultiply(image, &premultiplied, NULL)) goto done;
                image = premultiplied;
            }
            if (vips_resize(image, &scaled, (double) out_w / width,
                    "vscale", (double) out_h / height, NULL)) goto done;
            image = scaled;
            if (alpha) {
                if (vips_unpremultiply(image, &unpremultiplied, NULL)) goto done;
                image = unpremultiplied;
            }
            if (!bounded_dimensions(vips_image_get_width(image), vips_image_get_height(image)) ||
                vips_image_get_width(image) > (int) max_w || vips_image_get_height(image) > (int) max_h) goto done;
        }
    }

    if (target == JPEG && vips_image_hasalpha(image)) {
        double rgb[] = {header[7], header[8], header[9]};
        background = vips_array_double_new(rgb, 3);
        if (vips_flatten(image, &flat, "background", background, NULL)) goto done;
        image = flat;
    }
    if (save(image, target, quality, flags & 1)) goto done;

    struct stat info;
    if (stat(OUTPUT, &info) || info.st_size < 1 || info.st_size > MAX_BYTES) goto done;
    FILE *out = fopen(OUTPUT, "rb");
    if (!out) goto done;
    if (!copy_stream(out, stdout, MAX_BYTES) && !fflush(stdout)) status = 0;
    fclose(out);
done:
    if (background) vips_area_unref(VIPS_AREA(background));
    VIPS_UNREF(flat);
    VIPS_UNREF(unpremultiplied);
    VIPS_UNREF(scaled);
    VIPS_UNREF(premultiplied);
    VIPS_UNREF(colour);
    VIPS_UNREF(oriented);
    VIPS_UNREF(source);
    return status;
}

int main(int argc, char **argv) {
    if (argc != 1 || VIPS_INIT(argv[0])) return 1;
    vips_cache_set_max(0);
    vips_concurrency_set(1);
    unsigned char header[18];
    if (fread(header, 1, sizeof(header), stdin) != sizeof(header)) return 20;
    FILE *in = fopen(INPUT, "wb");
    if (!in) return 1;
    int failed = copy_stream(stdin, in, MAX_BYTES);
    if (fclose(in) || failed) return 20;
    int status = transform(header);
    vips_shutdown();
    return status;
}
