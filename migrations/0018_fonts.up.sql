-- Platform font library: fonts the BIB and certificate templates may use,
-- uploaded by platform administrators as needed (a family can hold just the
-- styles that are wanted, e.g. only Roboto Bold). They belong to the platform,
-- not to a workspace, so there is no tenant_id and no row-level security; every
-- signed-in user may read them, only platform administrators write.
--
-- The font data is kept in the row (a TrueType file is a few hundred KB and the
-- library holds tens of them): it is global, needs no per-tenant object, and
-- works with any storage driver. The dashboard fetches it with the session and
-- hands it to the browser as a FontFace.
CREATE TABLE fonts (
    id           UUID PRIMARY KEY,
    family       TEXT NOT NULL,
    -- active | disabled. A disabled font is not offered for new work but
    -- templates that already use it keep printing.
    status       TEXT NOT NULL DEFAULT 'active',
    license_note TEXT NOT NULL,
    created_by   UUID NOT NULL REFERENCES users(id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT fonts_status_ck CHECK (status IN ('active', 'disabled'))
);

CREATE UNIQUE INDEX fonts_family_uk ON fonts (lower(family));

CREATE TRIGGER fonts_set_updated_at
    BEFORE UPDATE ON fonts
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

CREATE TABLE font_files (
    font_id     UUID NOT NULL REFERENCES fonts(id) ON DELETE CASCADE,
    -- regular | bold | italic | bold_italic
    style       TEXT NOT NULL,
    data        BYTEA NOT NULL,
    size_bytes  INT NOT NULL,
    sha256      TEXT NOT NULL,
    -- OS/2 fsType: what the font's licence says about embedding it.
    fs_type     INT NOT NULL,
    glyph_count INT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (font_id, style),
    CONSTRAINT font_files_style_ck CHECK (style IN ('regular', 'bold', 'italic', 'bold_italic'))
);
