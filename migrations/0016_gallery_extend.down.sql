DROP INDEX IF EXISTS photo_tags_photo_bib_uk;
ALTER TABLE photo_tags DROP CONSTRAINT IF EXISTS photo_tags_source_ck;
ALTER TABLE photo_tags
    DROP COLUMN IF EXISTS box_x,
    DROP COLUMN IF EXISTS box_y,
    DROP COLUMN IF EXISTS box_w,
    DROP COLUMN IF EXISTS box_h;

DROP INDEX IF EXISTS photos_original_storage_uk;
DROP INDEX IF EXISTS photos_created_by_idx;
DROP INDEX IF EXISTS photos_album_created_idx;
DROP INDEX IF EXISTS photos_album_original_uk;
ALTER TABLE photos DROP CONSTRAINT IF EXISTS photos_ocr_status_ck;
ALTER TABLE photos
    DROP COLUMN IF EXISTS original_filename,
    DROP COLUMN IF EXISTS original_size,
    DROP COLUMN IF EXISTS size,
    DROP COLUMN IF EXISTS width,
    DROP COLUMN IF EXISTS height,
    DROP COLUMN IF EXISTS ocr_error;

DROP INDEX IF EXISTS albums_event_name_uk;
