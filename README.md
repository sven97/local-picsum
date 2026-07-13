# Local Picsum

Local Picsum is a self-hosted, Synology-friendly image placeholder service. It serves random, seeded, and ID-addressable images from folders selected in its local administrator UI.

## Run on Synology Container Manager

1. Copy this directory to the NAS and edit `compose.yaml`. Replace `/volume1/photo` with the one NAS root you want the service to read. Keep the container path as `/photos` and retain the `:ro` suffix.
2. In Container Manager, create a project from `compose.yaml`, then start it.
3. Visit `http://NAS_ADDRESS:8080/setup`, set an administrator password, and add the library folders. They are recursively indexed immediately and every six hours.

The `./data` volume holds the catalog, configuration, and login-session secret. Back it up with the rest of the project; do not mount it read-only.

## Image API

All image endpoints are public. The application accepts JPEG, PNG, and WebP source files.

```
GET /800                         # random square JPEG
GET /800/600                     # random 800×600 JPEG
GET /800/600.webp                # random WebP
GET /id/0123456789abcdef/800/600 # selected catalog ID
GET /seed/my-page/800/600        # stable image for a seed
GET /seed/my-page/800/600?grayscale&blur=2
GET /800/600?random=1            # cache-busting compatible URL
```

Images are centre-cropped and resized to the requested dimensions (1–10,000 pixels); upscaling is allowed. JPEG is the default output. `.jpg`, `.webp`, `grayscale`, and `blur=1..10` can be combined.

This v1 deliberately implements the image-delivery portion of Lorem Picsum only. `/v2/list` and `*/info` endpoints are not provided.

## Local development

```
go test ./...
docker compose up --build
```

## Container releases

Pushing a version tag such as `v1.0.0` publishes `linux/amd64` and `linux/arm64`
images to `ghcr.io/<GitHub owner>/local-picsum` with `1.0.0`, `1.0`, `1`, and
`latest` tags. The release workflow can also be run manually from GitHub Actions.
