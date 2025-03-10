package blob

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"strings"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/s3/s3manager"

	"github.com/caarlos0/log"
	"github.com/goreleaser/goreleaser/internal/artifact"
	"github.com/goreleaser/goreleaser/internal/extrafiles"
	"github.com/goreleaser/goreleaser/internal/semerrgroup"
	"github.com/goreleaser/goreleaser/internal/tmpl"
	"github.com/goreleaser/goreleaser/pkg/config"
	"github.com/goreleaser/goreleaser/pkg/context"
	"gocloud.dev/blob"
	"gocloud.dev/secrets"

	// Import the blob packages we want to be able to open.
	_ "gocloud.dev/blob/azureblob"
	_ "gocloud.dev/blob/gcsblob"
	_ "gocloud.dev/blob/s3blob"

	// import the secrets packages we want to be able to be used.
	_ "gocloud.dev/secrets/awskms"
	_ "gocloud.dev/secrets/azurekeyvault"
	_ "gocloud.dev/secrets/gcpkms"
)

func urlFor(ctx *context.Context, conf config.Blob) (string, error) {
	bucket, err := tmpl.New(ctx).Apply(conf.Bucket)
	if err != nil {
		return "", err
	}

	provider, err := tmpl.New(ctx).Apply(conf.Provider)
	if err != nil {
		return "", err
	}

	bucketURL := fmt.Sprintf("%s://%s", provider, bucket)
	if provider != "s3" {
		return bucketURL, nil
	}

	query := url.Values{}

	endpoint, err := tmpl.New(ctx).Apply(conf.Endpoint)
	if err != nil {
		return "", err
	}
	if endpoint != "" {
		query.Add("endpoint", endpoint)
		if conf.S3ForcePathStyle == nil {
			query.Add("s3ForcePathStyle", "true")
		} else {
			query.Add("s3ForcePathStyle", fmt.Sprintf("%t", *conf.S3ForcePathStyle))
		}
	}

	region, err := tmpl.New(ctx).Apply(conf.Region)
	if err != nil {
		return "", err
	}
	if region != "" {
		query.Add("region", region)
	}

	if conf.DisableSSL {
		query.Add("disableSSL", "true")
	}

	if len(query) > 0 {
		bucketURL = bucketURL + "?" + query.Encode()
	}

	return bucketURL, nil
}

// Takes goreleaser context(which includes artificats) and bucketURL for
// upload to destination (eg: gs://gorelease-bucket) using the given uploader
// implementation.
func doUpload(ctx *context.Context, conf config.Blob) error {
	folder, err := tmpl.New(ctx).Apply(conf.Folder)
	if err != nil {
		return err
	}
	folder = strings.TrimPrefix(folder, "/")

	bucketURL, err := urlFor(ctx, conf)
	if err != nil {
		return err
	}

	filter := artifact.Or(
		artifact.ByType(artifact.UploadableArchive),
		artifact.ByType(artifact.UploadableBinary),
		artifact.ByType(artifact.UploadableSourceArchive),
		artifact.ByType(artifact.Checksum),
		artifact.ByType(artifact.Signature),
		artifact.ByType(artifact.Certificate),
		artifact.ByType(artifact.LinuxPackage),
		artifact.ByType(artifact.SBOM),
	)
	if len(conf.IDs) > 0 {
		filter = artifact.And(filter, artifact.ByIDs(conf.IDs...))
	}

	up := &productionUploader{}
	if err := up.Open(ctx, bucketURL, &conf); err != nil {
		return handleError(err, bucketURL)
	}
	defer up.Close()

	g := semerrgroup.New(ctx.Parallelism)
	for _, artifact := range ctx.Artifacts.Filter(filter).List() {
		artifact := artifact
		g.Go(func() error {
			// TODO: replace this with ?prefix=folder on the bucket url
			dataFile := artifact.Path
			uploadFile := path.Join(folder, artifact.Name)

			return uploadData(ctx, conf, up, dataFile, uploadFile, bucketURL)
		})
	}

	files, err := extrafiles.Find(ctx, conf.ExtraFiles)
	if err != nil {
		return err
	}
	for name, fullpath := range files {
		name := name
		fullpath := fullpath
		g.Go(func() error {
			uploadFile := path.Join(folder, name)
			return uploadData(ctx, conf, up, fullpath, uploadFile, bucketURL)
		})
	}

	return g.Wait()
}

func uploadData(ctx *context.Context, conf config.Blob, up uploader, dataFile, uploadFile, bucketURL string) error {
	data, err := getData(ctx, conf, dataFile)
	if err != nil {
		return err
	}

	if err := up.Upload(ctx, uploadFile, data); err != nil {
		return handleError(err, bucketURL)
	}
	return nil
}

// errorContains check if error contains specific string.
func errorContains(err error, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(err.Error(), sub) {
			return true
		}
	}
	return false
}

func handleError(err error, url string) error {
	switch {
	case errorContains(err, "NoSuchBucket", "ContainerNotFound", "notFound"):
		return fmt.Errorf("provided bucket does not exist: %s: %w", url, err)
	case errorContains(err, "NoCredentialProviders"):
		return fmt.Errorf("check credentials and access to bucket: %s: %w", url, err)
	case errorContains(err, "InvalidAccessKeyId"):
		return fmt.Errorf("aws access key id you provided does not exist in our records: %w", err)
	case errorContains(err, "AuthenticationFailed"):
		return fmt.Errorf("azure storage key you provided is not valid: %w", err)
	case errorContains(err, "invalid_grant"):
		return fmt.Errorf("google app credentials you provided is not valid: %w", err)
	case errorContains(err, "no such host"):
		return fmt.Errorf("azure storage account you provided is not valid: %w", err)
	case errorContains(err, "ServiceCode=ResourceNotFound"):
		return fmt.Errorf("missing azure storage key for provided bucket %s: %w", url, err)
	default:
		return fmt.Errorf("failed to write to bucket: %w", err)
	}
}

func getData(ctx *context.Context, conf config.Blob, path string) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return data, fmt.Errorf("failed to open file %s: %w", path, err)
	}
	if conf.KMSKey == "" {
		return data, nil
	}
	keeper, err := secrets.OpenKeeper(ctx, conf.KMSKey)
	if err != nil {
		return data, fmt.Errorf("failed to open kms %s: %w", conf.KMSKey, err)
	}
	defer keeper.Close()
	data, err = keeper.Encrypt(ctx, data)
	if err != nil {
		return data, fmt.Errorf("failed to encrypt with kms: %w", err)
	}
	return data, err
}

// uploader implements upload.
type uploader interface {
	io.Closer
	Open(ctx *context.Context, url string, conf *config.Blob) error
	Upload(ctx *context.Context, path string, data []byte) error
}

// productionUploader actually do upload to.
type productionUploader struct {
	bucket *blob.Bucket
	conf   *config.Blob
}

func (u *productionUploader) Close() error {
	if u.bucket == nil {
		return nil
	}
	return u.bucket.Close()
}

func (u *productionUploader) Open(ctx *context.Context, bucket string, conf *config.Blob) error {
	log.WithFields(log.Fields{
		"bucket": bucket,
	}).Debug("uploading")

	conn, err := blob.OpenBucket(ctx, bucket)
	if err != nil {
		return err
	}
	u.bucket = conn
	u.conf = conf
	return nil
}

func (u *productionUploader) Upload(ctx *context.Context, filepath string, data []byte) error {
	log.WithField("path", filepath).Info("uploading")
	opts := &blob.WriterOptions{
		ContentDisposition: "attachment; filename=" + path.Base(filepath),
	}
	if u.conf.ACL != "" {
		opts.BeforeWrite = func(asFunc func(interface{}) bool) error {
			req := &s3manager.UploadInput{}
			ok := asFunc(&req)
			if !ok {
				return errors.New("invalid s3 type")
			}
			req.ACL = aws.String(u.conf.ACL)
			return nil
		}
	}

	w, err := u.bucket.NewWriter(ctx, filepath, opts)
	if err != nil {
		return err
	}
	defer func() { _ = w.Close() }()
	if _, err = w.Write(data); err != nil {
		return err
	}
	return w.Close()
}
