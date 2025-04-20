package backup

import (
	"compress/gzip"
	"context"
	"fmt"
	"io" // Ensure io is imported
	"log"
	"os/exec" // Keep os/exec for running git
	"strings"

	// Use the actual module path defined in your go.mod file
	"git-monitor-app/config" // Adjust if your module name is different

	"github.com/aws/aws-sdk-go-v2/aws"
	awsConfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// --- Gzip Pipe Helper ---

// GzipPipe sets up an in-memory pipe where data read from 'src' is gzipped
// and can be read from the returned io.ReadCloser. Compression happens
// in a background goroutine. Uses io.Pipe() for in-process piping.
func GzipPipe(src io.Reader) (io.ReadCloser, error) {
	// Use io.Pipe() for in-memory pipe connecting goroutines
	pr, pw := io.Pipe() // pr is *io.PipeReader, pw is *io.PipeWriter

	gzipWriter := gzip.NewWriter(pw) // gzip writes TO the pipe's writer end

	// Goroutine to read from input source -> gzip -> write to pipe
	go func() {
		// Setup defers to close the writer ends when the goroutine finishes
		var err error // Variable to store final error status for CloseWithError
		defer func() {
			// Must close gzipWriter first to flush compressed data to pw
			if cerr := gzipWriter.Close(); err == nil && cerr != nil {
				err = cerr // Record gzip close error if no prior error
			}
			// Close the pipe writer, propagating any error that occurred during copy/gzip
			pw.CloseWithError(err)
		}()

		// Copy data from the source (e.g., command stdout) to the gzip writer
		_, err = io.Copy(gzipWriter, src) // Assign error to the 'err' variable declared above
		if err != nil {
			log.Printf("Backup Error: during gzip pipe copy: %v", err)
			// Error is stored in 'err' and will be used by pw.CloseWithError in defer
		}
		log.Println("Backup: Gzip goroutine finished.")
	}()

	// Return the *reader* end of the pipe (*io.PipeReader implements io.ReadCloser)
	return pr, nil
}

// --- Backup Functionality ---

// RunBackup performs the backup of a specific commit to S3/Wasabi.
func RunBackup(repoPath, commitHash string, cfg *config.BackupConfig) error {
	log.Printf("Backup: Starting backup process for commit %s", commitHash)

	// Basic validation of essential config
	if cfg.Bucket == "" {
		return fmt.Errorf("backup config error: S3 bucket name is required")
	}

	// --- Configure AWS SDK ---
	log.Println("Backup: Configuring S3 client...")
	sdkConfigOptions := []func(*awsConfig.LoadOptions) error{}

	// 1. Custom Endpoint Resolver (Essential for Wasabi/S3 Compatible)
	if cfg.EndpointURL != "" {
		customResolver := aws.EndpointResolverWithOptionsFunc( // Use newer resolver type
			func(service, region string, options ...interface{}) (aws.Endpoint, error) {
				if service == s3.ServiceID {
					ep := aws.Endpoint{
						URL:           cfg.EndpointURL,
						SigningRegion: cfg.Region, // Use region from config for signing
					}
					return ep, nil
				}
				return aws.Endpoint{}, &aws.EndpointNotFoundError{}
			})
		sdkConfigOptions = append(sdkConfigOptions, awsConfig.WithEndpointResolverWithOptions(customResolver))
	}

	// 2. Region
	if cfg.Region != "" {
		sdkConfigOptions = append(sdkConfigOptions, awsConfig.WithRegion(cfg.Region))
	}

	// 3. Credentials
	if cfg.AccessKeyID != "" && cfg.SecretKey != "" {
		log.Println("Backup: Using static credentials from config file.")
		staticCreds := credentials.NewStaticCredentialsProvider(cfg.AccessKeyID, cfg.SecretKey, "")
		sdkConfigOptions = append(sdkConfigOptions, awsConfig.WithCredentialsProvider(staticCreds))
	} else {
		log.Println("Backup: Using default AWS credential chain.")
	}

	// Load the final configuration
	sdkConfig, err := awsConfig.LoadDefaultConfig(context.TODO(), sdkConfigOptions...)
	if err != nil {
		return fmt.Errorf("failed to load AWS SDK config: %w", err)
	}
	if cfg.Region != "" && sdkConfig.Region != cfg.Region {
		sdkConfig.Region = cfg.Region
		log.Printf("Backup: Explicitly setting region in loaded SDK config: %s", cfg.Region)
	}

	// Create S3 client
	s3Client := s3.NewFromConfig(sdkConfig)

	// --- Construct S3 Key ---
	backupFilename := fmt.Sprintf("commit-%s.tar.gz", commitHash)
	s3Key := backupFilename
	if cfg.Prefix != "" {
		cleanPrefix := strings.Trim(cfg.Prefix, "/")
		if cleanPrefix != "" {
			s3Key = fmt.Sprintf("%s/%s", cleanPrefix, backupFilename)
		}
	}
	s3Path := fmt.Sprintf("s3://%s/%s", cfg.Bucket, s3Key)
	log.Printf("Backup: Target S3 Key: %s\n", s3Path)

	// --- Create Archive Stream ---
	log.Println("Backup: Creating git archive stream...")
	cmdArchive := exec.Command("git", "-C", repoPath, "archive", "--format=tar", commitHash)
	stdoutPipe, err := cmdArchive.StdoutPipe() // Get pipe BEFORE starting command
	if err != nil {
		return fmt.Errorf("failed to get stdout pipe for git archive: %w", err)
	}
	var stderr strings.Builder
	cmdArchive.Stderr = &stderr

	// Start git archive (doesn't wait)
	if err := cmdArchive.Start(); err != nil {
		// Ensure stdoutPipe is closed if command start fails? Or let caller handle?
		// Closing stdoutPipe isn't directly possible, maybe kill process?
		return fmt.Errorf("failed to start git archive: %w", err)
	}

	// --- Setup Gzip Pipe ---
	gzipReader, err := GzipPipe(stdoutPipe) // Pass command's stdout pipe as the source reader
	if err != nil {
		_ = cmdArchive.Process.Kill() // Attempt to clean up started process
		_ = cmdArchive.Wait()         // Wait to release resources
		return fmt.Errorf("failed to setup gzip pipe: %w", err)
	}
	// Defer Close on the reader end of the gzip pipe (*io.PipeReader).
	// This is crucial. When the S3 upload finishes (or errors), this Close()
	// will signal the goroutine inside GzipPipe (via io.Copy returning ErrClosedPipe)
	// allowing it to clean up.
	defer gzipReader.Close()

	// --- Upload to S3 ---
	log.Println("Backup: Starting S3 upload...")
	_, uploadErr := s3Client.PutObject(context.TODO(), &s3.PutObjectInput{
		Bucket: aws.String(cfg.Bucket),
		Key:    aws.String(s3Key),
		Body:   gzipReader, // Read directly from the gzip reader pipe
	})

	// Wait for the 'git archive' command to finish *after* upload attempt
	// Reading from gzipReader inside PutObject drives the flow. The command
	// will complete once its stdout is fully consumed or the pipe breaks.
	archiveErr := cmdArchive.Wait()

	// Check for errors, prioritizing S3 upload error
	if uploadErr != nil {
		// S3 upload failed
		if archiveErr != nil {
			log.Printf("Backup: git archive command also failed (stderr: %s): %v", stderr.String(), archiveErr)
		}
		// The error might be context canceled if the pipe closed due to archiveErr, or the S3 error itself
		return fmt.Errorf("failed to upload to S3 (%s): %w", s3Path, uploadErr)
	}

	// Check git archive error if S3 upload seemed okay
	if archiveErr != nil {
		// This means S3 upload finished, but the source command reported an error.
		// This could indicate incomplete data, though unlikely if S3 returned success.
		return fmt.Errorf("git archive command failed after upload (stderr: %s): %w", stderr.String(), archiveErr)
	}

	log.Printf("Backup: Upload SUCCEEDED for %s", s3Path)
	return nil // Success
}
