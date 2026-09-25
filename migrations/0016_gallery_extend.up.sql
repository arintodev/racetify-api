-- Media Gallery, second pass over 0009 (docs/media-gallery-integration.md).
-- photos.created_by from 0009 is the photographer (the logged-in user who
-- uploaded); no separate uploaded_by column is needed.

-- Album names are unique per event, case-insensitively, so the dropdown of
-- albums in the upload dialog is unambiguous.
CREATE UNIQUE INDEX albums_event_name_uk ON albums (event_id, lower(name));

ALTER TABLE photos
    -- Name + size of the file the photographer picked, before the browser
    -- resized it: what duplicate detection compares.
    ADD COLUMN original_filename TEXT   NOT NULL DEFAULT '',
    ADD COLUMN original_size     BIGINT NOT NULL DEFAULT 0,
    -- The stored (resized) file.
    ADD COLUMN size              BIGINT NOT NULL DEFAULT 0,
    ADD COLUMN width             INTEGER NOT NULL DEFAULT 0,
    ADD COLUMN height            INTEGER NOT NULL DEFAULT 0,
    -- Why ocr_status is 'failed'.
    ADD COLUMN ocr_error         TEXT NULL;

ALTER TABLE photos
    ADD CONSTRAINT photos_ocr_status_ck CHECK (ocr_status IN ('pending', 'processed', 'failed'));

CREATE UNIQUE INDEX photos_album_original_uk
    ON photos (album_id, original_filename, original_size)
    WHERE original_filename <> '';

-- Completing the same upload twice must not create a second photo.
CREATE UNIQUE INDEX photos_original_storage_uk ON photos (original_storage_id);

-- Keyset pagination of an album's photos, newest first.
CREATE INDEX photos_album_created_idx ON photos (album_id, created_at DESC, id DESC);
CREATE INDEX photos_created_by_idx ON photos (created_by);

-- Where OCR found a tag, as fractions of the photo's width/height. Manual
-- tags have no box.
ALTER TABLE photo_tags
    ADD COLUMN box_x NUMERIC(5,4) NULL,
    ADD COLUMN box_y NUMERIC(5,4) NULL,
    ADD COLUMN box_w NUMERIC(5,4) NULL,
    ADD COLUMN box_h NUMERIC(5,4) NULL;

ALTER TABLE photo_tags
    ADD CONSTRAINT photo_tags_source_ck CHECK (source IN ('ocr', 'manual'));

-- One tag per BIB on a photo.
CREATE UNIQUE INDEX photo_tags_photo_bib_uk ON photo_tags (photo_id, bib_string);
