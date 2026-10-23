CREATE TABLE user_images (
    user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    purpose TEXT NOT NULL,
    storage_key TEXT NOT NULL,
    content_type TEXT NOT NULL,
    width INTEGER NOT NULL,
    height INTEGER NOT NULL,
    byte_size BIGINT NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (user_id, purpose),
    CONSTRAINT user_images_purpose_check CHECK (purpose IN ('profile', 'cover')),
    CONSTRAINT user_images_content_type_check CHECK (content_type = 'image/jpeg'),
    CONSTRAINT user_images_width_check CHECK (width > 0),
    CONSTRAINT user_images_height_check CHECK (height > 0),
    CONSTRAINT user_images_byte_size_check CHECK (byte_size > 0),
    CONSTRAINT user_images_storage_key_unique UNIQUE (storage_key),
    CONSTRAINT user_images_storage_key_format CHECK (
        storage_key ~ '^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\.jpg$'
    )
);
