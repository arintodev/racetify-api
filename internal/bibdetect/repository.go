package bibdetect

import (
	"github.com/racetify/racetify-api/internal/platform/database"
)

type Repository struct {
	db *database.DB
}

func NewRepository(db *database.DB) *Repository { return &Repository{db: db} }