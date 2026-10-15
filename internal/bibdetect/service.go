package bibdetect

import (
    "github.com/racetify/racetify-api/internal/platform/database"
    "github.com/racetify/racetify-api/internal/storage"
    "github.com/racetify/racetify-api/internal/gallery"
    "github.com/racetify/racetify-api/internal/jobqueue"
)

type Service struct {
	db      *database.DB
	repo    *Repository
	storage *storage.Service
	bibOCR  *BibOCRClient
	gallery *gallery.Service
	queue   *jobqueue.Queue
}

func NewService(db *database.DB, repo *Repository, storageService *storage.Service, bibOCR *BibOCRClient, galleryService *gallery.Service, queue *jobqueue.Queue) *Service {
	return &Service{
		db: db, repo: repo, storage: storageService, bibOCR: bibOCR, gallery: galleryService, queue: queue,
	}
}
