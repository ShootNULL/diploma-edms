package config

import (
	"log"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

var minioClient *minio.Client

var minioBucketName = getEnv("MINIO_BUCKET_NAME")

func InitMinio() {
	endpoint := getEnv("MINIO_ENDPOINT")
	accessKeyID := getEnv("MINIO_ACCESSKEY_ID")
	secretAccessKey := getEnv("MINIO_ACCESSKEY_SECRET")
	useSSL := false

	// Initialize minio client object.
	var err error
	minioClient, err = minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKeyID, secretAccessKey, ""),
		Secure: useSSL,
	})
	if err != nil || minioClient == nil {
		panic(err)
	}

	log.Printf("minio client created")
	log.Printf("%#v\n", minioClient) // minioClient is now setup

}

func MinioClient() *minio.Client {
	return minioClient
}

func MinioBucketName() string {
	return minioBucketName
}
