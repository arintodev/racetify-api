# Portal Photo Search API

The **Runner Portal** exposes the following endpoint to search public photos of
an event:

```
GET /api/v1/public/events/{slug}/gallery/photos
```

## Query Parameters

| Parameter | Required | Description |
|-----------|----------|-------------|
| `bib` | No | Search photos by Bib number (existing behaviour).
| `face_id` | No | Search photos by the face ID returned by the Face API. **Requires
authentication** and the supplied `face_id` must belong to the logged‑in user.

### Authentication & Permissions

* The endpoint is publicly readable when `face_id` is **not** supplied.
* When `face_id` is present the request **must** contain a valid
  user‑session (JWT or cookie). If the session is missing, the API returns
  **401 Unauthorized**.
* If the `face_id` does not belong to the authenticated user, the API returns
  **403 Forbidden**.

### Error Responses

* `401 Unauthorized` – No or invalid user session while requesting with
  `face_id`.
* `403 Forbidden` – Authenticated user does not own the supplied `face_id`.
* `400 Bad Request` – Malformed `face_id`.

### Example

```http
GET /api/v1/public/events/race-2026/gallery/photos?face_id=123e4567-e89b-12d3-a456-426614174000
Authorization: Bearer <jwt-token>
```

The response will contain the list of public photos that contain the face
identified by the given `face_id`.
