// Package storage wraps MinIO/S3-compatible object storage for evidence,
// reports and scenario assets. PostgreSQL stores only object references;
// clients receive short-lived signed URLs.
//
// Implemented in phase 8.
package storage
