package main

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/bootdotdev/learn-file-storage-s3-golang-starter/internal/auth"
	"github.com/google/uuid"
)

type Stream struct {
	DisplayAspectRatio string `json:"display_aspect_ratio"`
}

type Video struct {
	Streams []Stream `json:"streams"`
}

func (cfg *apiConfig) handlerUploadVideo(w http.ResponseWriter, r *http.Request) {
	videoIDString := r.PathValue("videoID")
	videoID, err := uuid.Parse(videoIDString)
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid ID", err)
		return
	}

	token, err := auth.GetBearerToken(r.Header)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't find JWT", err)
		return
	}

	userID, err := auth.ValidateJWT(token, cfg.jwtSecret)
	if err != nil {
		respondWithError(w, http.StatusUnauthorized, "Couldn't validate JWT", err)
		return
	}

	video, err := cfg.db.GetVideo(videoID)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't find video", err)
		return
	}
	if video.UserID != userID {
		respondWithError(w, http.StatusUnauthorized, "Not authorized to update this video", nil)
		return
	}

	const maxMemory = 1 << 30 // 1 GB
	r.Body = http.MaxBytesReader(w, r.Body, maxMemory)

	file, header, err := r.FormFile("video")
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Unable to parse form file", err)
		return
	}
	defer file.Close()

	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil {
		respondWithError(w, http.StatusBadRequest, "Invalid Content-Type", err)
		return
	}
	if mediaType != "video/mp4" {
		respondWithError(w, http.StatusBadRequest, "Invalid file type", nil)
		return
	}

	dst, err := os.CreateTemp("", "tubely-upload*.mp4")
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Unable to create temp file on server", err)
		return
	}
	defer os.Remove(dst.Name())
	defer dst.Close()

	if _, err = io.Copy(dst, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Error saving temp file", err)
		return
	}

	prefix, err := getVideoAspectRatio(dst.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not get ratio", err)
		return
	}

	bodypath, err := processVideoForFastStart(dst.Name())
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could convert video", err)
		return
	}
	defer os.Remove(bodypath)

	body, err := os.Open(bodypath)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could find converted video", err)
		return
	}
	defer body.Close()

	body.Seek(0, io.SeekStart)

	ranID := make([]byte, 32)
	rand.Read(ranID)
	ranString := prefix + "/" + base64.RawURLEncoding.EncodeToString(ranID) + ".mp4"

	_, err = cfg.s3Client.PutObject(r.Context(), &s3.PutObjectInput{
		Bucket:      &cfg.s3Bucket,
		Key:         &ranString,
		ContentType: &mediaType,
		Body:        body,
	})
	if _, err = io.Copy(dst, file); err != nil {
		respondWithError(w, http.StatusInternalServerError, "Could not upload object", err)
		return
	}

	url := fmt.Sprintf("%v/%v", cfg.s3CfDistribution, ranString)
	video.VideoURL = &url

	err = cfg.db.UpdateVideo(video)
	if err != nil {
		respondWithError(w, http.StatusInternalServerError, "Couldn't update video", err)
		return
	}

	respondWithJSON(w, http.StatusOK, video)
}

func getVideoAspectRatio(filePath string) (string, error) {
	cmd := exec.Command("ffprobe", "-v", "error", "-print_format", "json", "-show_streams", filePath)
	buffer := bytes.Buffer{}
	cmd.Stdout = &buffer
	err := cmd.Run()
	if err != nil {
		return "", err
	}

	var video Video
	if err := json.Unmarshal(buffer.Bytes(), &video); err != nil {
		return "", err
	}

	switch video.Streams[0].DisplayAspectRatio {
	case "16:9":
		return "landscape", nil
	case "9:16":
		return "portrait", nil
	default:
		return "other", nil
	}
}

func processVideoForFastStart(filePath string) (string, error) {
	outpath := filePath + ".processing"
	cmd := exec.Command("ffmpeg", "-i", filePath, "-c", "copy", "-movflags", "faststart", "-f", "mp4", outpath)
	err := cmd.Run()
	if err != nil {
		return "", err
	}

	return outpath, nil
}
