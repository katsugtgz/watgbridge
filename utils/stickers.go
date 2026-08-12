package utils

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"

	// "image/color"
	"image/draw"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"

	"watgbridge/state"

	"github.com/watgbridge/tgsconverter/libtgsconverter"
	"github.com/watgbridge/webp"
	"go.uber.org/zap"
)

func TGSConvertToWebp(tgsStickerData []byte, updateId int64) ([]byte, error) {
	logger := state.State.Logger
	defer logger.Sync()
	opt := libtgsconverter.NewConverterOptions()
	opt.SetExtension("webp")
	var (
		quality float32 = 100
		fps     uint    = 30
	)
	for quality > 2 && fps > 5 {
		logger.Debug("trying to convert tgs to webp",
			zap.Int64("updateId", updateId),
			zap.Float32("quality", quality),
			zap.Uint("fps", fps),
		)
		opt.SetFPS(fps)
		opt.SetWebpQuality(quality)
		webpStickerData, err := libtgsconverter.ImportFromData(tgsStickerData, opt)
		if err != nil {
			return nil, err
		} else if len(webpStickerData) < 1024*1024 {
			if outputDataWithExif, err := WebpWriteExifData(webpStickerData); err == nil {
				return outputDataWithExif, nil
			}
			return webpStickerData, nil
		}
		quality /= 2
		fps = uint(float32(fps) / 1.5)
	}
	return nil, fmt.Errorf("sticker has a lot of data which cannot be handled by WhatsApp")
}

func WebmConvertToWebp(webmStickerData []byte, updateId int64) ([]byte, error) {
	logger := state.State.Logger
	defer logger.Sync()

	ffmpegExec := state.State.Config.FfmpegExecutable
	if ffmpegExec == "" {
		ffmpegExec = "ffmpeg"
	}

	tempInput, err := os.CreateTemp("", "webm_input_*.webm")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp input file for webm: %w", err)
	}
	defer os.Remove(tempInput.Name())

	if _, err := tempInput.Write(webmStickerData); err != nil {
		tempInput.Close()
		return nil, fmt.Errorf("failed to write temp input file for webm: %w", err)
	}
	tempInput.Close()

	tempOutput, err := os.CreateTemp("", "webp_output_*.webp")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp output file for webp: %w", err)
	}
	defer os.Remove(tempOutput.Name())
	tempOutput.Close()

	var (
		quality = 75
		fps     = 15
	)

	for quality >= 30 && fps >= 8 {
		vf := fmt.Sprintf("fps=%d,scale=512:512:force_original_aspect_ratio=decrease:force_divisible_by=2,pad=512:512:(ow-iw)/2:(oh-ih)/2:color=#00000000,format=rgba", fps)
		cmd := exec.Command(ffmpegExec,
			"-i", tempInput.Name(),
			"-c:v", "libwebp",
			"-loop", "0",
			"-preset", "default",
			"-an",
			"-vsync", "0",
			"-quality", strconv.Itoa(quality),
			"-compression_level", "6",
			"-vf", vf,
			"-f", "webp",
			"-y",
			tempOutput.Name(),
		)

		var stderr bytes.Buffer
		cmd.Stderr = &stderr

		if err := cmd.Run(); err == nil {
			outputBytes, readErr := os.ReadFile(tempOutput.Name())
			if readErr == nil && len(outputBytes) > 0 && len(outputBytes) <= 500*1024 {
				if outputDataWithExif, err := WebpWriteExifData(outputBytes); err == nil {
					return outputDataWithExif, nil
				} else {
					logger.Debug("failed to write exif to webp", zap.Error(err))
				}
				return outputBytes, nil
			}
		} else {
			logger.Debug("ffmpeg webm conversion attempt failed",
				zap.Int("quality", quality),
				zap.Int("fps", fps),
				zap.String("stderr", stderr.String()),
			)
		}

		quality -= 15
		fps -= 2
	}

	return nil, fmt.Errorf("webm sticker could not be converted under WhatsApp size limit")
}

func WebpImagePad(inputData []byte, wPad, hPad int, updateId int64) ([]byte, error) {
	inputImage, err := webp.DecodeRGBA(inputData)
	if err != nil {
		return nil, fmt.Errorf("failed to decode web image: %w", err)
	}

	var (
		wOffset = wPad / 2
		hOffset = hPad / 2
	)

	outputWidth := inputImage.Bounds().Dx() + wPad
	outputHeight := inputImage.Bounds().Dy() + hPad

	outputImage := image.NewRGBA(image.Rect(0, 0, outputWidth, outputHeight))
	draw.Draw(outputImage, image.Rect(wOffset, hOffset, outputWidth-wOffset, outputHeight-hOffset), inputImage, image.Point{}, draw.Src)

	outputBytes, err := webp.EncodeRGBA(outputImage, 100)
	if err != nil {
		return nil, fmt.Errorf("failed to encode padded data into Webp: %w", err)
	}

	if outputData, err := WebpWriteExifData(outputBytes); err == nil {
		return outputData, nil
	}

	return outputBytes, nil
}

func AnimatedWebpConvertToWebm(inputData []byte, updateId string) ([]byte, error) {
	var (
		logger = state.State.Logger

		currPath   = filepath.Join("downloads", updateId)
		inputPath  = filepath.Join(currPath, "input.webp")
		outputPath = filepath.Join(currPath, "output.webm")
	)
	defer logger.Sync()

	if err := os.MkdirAll(currPath, os.ModePerm); err != nil {
		return nil, err
	}
	defer os.RemoveAll(currPath)

	if err := os.WriteFile(inputPath, inputData, os.ModePerm); err != nil {
		return nil, err
	}

	// Convert WebP to WEBM with VP9 codec
	// Following Telegram's requirements:
	// - One side must be exactly 512px (other can be 512px or less)
	// - Max 30 FPS
	// - Loop for optimal UX
	// - Max 256KB file size
	// - VP9 codec
	// - No audio stream
	logger.Debug("Starting WEBM conversion",
		zap.String("inputPath", inputPath),
		zap.String("outputPath", outputPath),
	)

	// First convert WebP to GIF using ImageMagick (which handles animated WebP better)
	tempGifPath := filepath.Join(currPath, "temp.gif")

	convertCmd := exec.Command("convert",
		inputPath,
		"-coalesce",  // Ensure all frames are complete
		"-loop", "0", // Loop infinitely
		tempGifPath,
	)

	var convertStderr bytes.Buffer
	convertCmd.Stderr = &convertStderr

	if err := convertCmd.Run(); err != nil {
		logger.Debug("failed to convert webp to gif with ImageMagick",
			zap.Error(err),
			zap.String("stderr", convertStderr.String()),
		)
		return nil, err
	}

	logger.Debug("WebP to GIF conversion completed, now converting to WEBM")

	ffmpegExec := state.State.Config.FfmpegExecutable
	if ffmpegExec == "" {
		ffmpegExec = "ffmpeg"
	}

	// Now convert GIF to WEBM with VP9 codec
	cmd := exec.Command(ffmpegExec,
		"-i", tempGifPath,
		"-c:v", "libvpx-vp9", // VP9 codec
		"-an",                                                                                       // No audio stream
		"-vf", "scale=512:512:force_original_aspect_ratio=decrease,pad=512:512:(ow-iw)/2:(oh-ih)/2", // Scale to 512px maintaining aspect ratio
		"-r", "30", // Max 30 FPS
		"-b:v", "0", // Use CRF mode
		"-crf", "30", // Quality setting (lower = better quality, higher file size)
		"-deadline", "good", // Encoding speed vs quality trade-off
		"-cpu-used", "2", // CPU usage (0-5, higher = faster encoding)
		"-fs", "256K", // Max file size 256KB
		"-y", // Overwrite output file
		outputPath,
	)

	// Capture stderr for debugging
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		logger.Debug("failed to run ffmpeg command for webm conversion",
			zap.Error(err),
			zap.String("stderr", stderr.String()),
		)
		return nil, err
	}

	logger.Debug("WEBM conversion completed successfully")

	return os.ReadFile(outputPath)
}

// Fallback function to convert to GIF if WEBM conversion fails
func AnimatedWebpConvertToGif(inputData []byte, updateId string) ([]byte, error) {
	logger := state.State.Logger
	defer logger.Sync()

	cmd := exec.Command("convert",
		"webp:-",
		"-loop", "0",
		"-dispose", "previous",
		"gif:-",
	)

	var outputBuf, stderr bytes.Buffer
	cmd.Stdin = bytes.NewReader(inputData)
	cmd.Stdout = &outputBuf
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		logger.Debug("convert command failed",
			zap.Error(err),
			zap.String("stderr", stderr.String()),
		)
		return nil, err
	}

	return outputBuf.Bytes(), nil
}

func WebpWriteExifData(inputData []byte) ([]byte, error) {
	var (
		cfg           = state.State.Config
		logger        = state.State.Logger
		startingBytes = []byte{0x49, 0x49, 0x2A, 0x00, 0x08, 0x00, 0x00, 0x00, 0x01, 0x00, 0x41, 0x57, 0x07, 0x00}
		endingBytes   = []byte{0x16, 0x00, 0x00, 0x00}
		b             bytes.Buffer
	)
	defer logger.Sync()

	exifFile, err := os.CreateTemp("", "raw*.exif")
	if err != nil {
		return nil, fmt.Errorf("failed to create exif file: %w", err)
	}
	defer os.Remove(exifFile.Name())

	inputFile, err := os.CreateTemp("", "input")
	if err != nil {
		return nil, fmt.Errorf("failed to create input data file: %w", err)
	}
	defer os.Remove(inputFile.Name())

	if _, err := b.Write(startingBytes); err != nil {
		return nil, err
	}

	jsonData := map[string]any{
		"sticker-pack-id":        "watgbridge.akshettrj.com.github.",
		"sticker-pack-name":      cfg.WhatsApp.StickerMetadata.PackName,
		"sticker-pack-publisher": cfg.WhatsApp.StickerMetadata.AuthorName,
		"emojis":                 []string{"😀"},
	}
	jsonBytes, err := json.Marshal(jsonData)
	if err != nil {
		return nil, err
	}

	jsonLength := (uint32)(len(jsonBytes))
	lenBuffer := make([]byte, 4)
	binary.LittleEndian.PutUint32(lenBuffer, jsonLength)

	if _, err := b.Write(lenBuffer); err != nil {
		return nil, err
	}
	if _, err := b.Write(endingBytes); err != nil {
		return nil, err
	}
	if _, err := b.Write(jsonBytes); err != nil {
		return nil, err
	}

	if _, err := exifFile.Write(b.Bytes()); err != nil {
		return nil, err
	}
	exifFile.Close()

	if _, err := inputFile.Write(inputData); err != nil {
		return nil, err
	}
	inputFile.Close()

	outputFile, err := os.CreateTemp("", "exif_out*.webp")
	if err != nil {
		return nil, fmt.Errorf("failed to create exif output file: %w", err)
	}
	defer os.Remove(outputFile.Name())
	outputFile.Close()

	cmd := exec.Command("webpmux",
		"-set", "exif", exifFile.Name(),
		inputFile.Name(),
		"-o", outputFile.Name(),
	)

	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		logger.Debug("failed to run webpmux command",
			zap.Error(err),
			zap.String("stderr", stderr.String()),
		)
		return nil, err
	}

	return os.ReadFile(outputFile.Name())
}

func GenerateVideoThumbnail(videoData []byte) ([]byte, error) {
	logger := state.State.Logger
	defer logger.Sync()

	ffmpegExec := state.State.Config.FfmpegExecutable
	if ffmpegExec == "" {
		ffmpegExec = "ffmpeg"
	}

	tempFile, err := os.CreateTemp("", "thumb_input_*")
	if err != nil {
		return nil, fmt.Errorf("failed to create temp file for thumbnail: %w", err)
	}
	defer os.Remove(tempFile.Name())

	if _, err := tempFile.Write(videoData); err != nil {
		tempFile.Close()
		return nil, fmt.Errorf("failed to write temp file for thumbnail: %w", err)
	}
	tempFile.Close()

	cmd := exec.Command(ffmpegExec,
		"-i", tempFile.Name(),
		"-vframes", "1",
		"-vf", "scale=160:160:force_original_aspect_ratio=decrease",
		"-f", "image2",
		"-c:v", "mjpeg",
		"-q:v", "8",
		"-y",
		"-",
	)

	var outputBuf, stderr bytes.Buffer
	cmd.Stdout = &outputBuf
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		logger.Debug("ffmpeg thumbnail generation failed",
			zap.Error(err),
			zap.String("stderr", stderr.String()),
		)
		return nil, err
	}

	return outputBuf.Bytes(), nil
}
