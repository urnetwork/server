// Concrete read-only native kernel/image/release capture is one component of
// matched-kernel-source. It never admits a class or substitutes for source-Git
// provenance, actual trial operations, or the independent operator review.
package competitionagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
)

// The concrete native host implements these exact protected observations.
// Tests replace kernel/image/file I/O, not release or identity validation.
type nativeQualificationReleaseHost interface {
	NativeClassKernel(context.Context, RuntimeResourceBoundarySettings) (RuntimeNativeKernelIdentity, error)
	NativeClassImage(context.Context, RuntimeResourceBoundary, string, RuntimeResourceBoundarySettings) ([]byte, error)
	Document(Pin, int64) ([]byte, error)
	Executable(context.Context, Pin, int64) (int64, error)
}

// Hashes retain the private raw observations without exposing environment or
// host strings to logs. No passed/accepted field exists in this intermediate.
type nativeQualificationKernelReleaseFacts struct {
	Schema                 int                         `json:"schema"`
	Kind                   string                      `json:"kind"`
	RuntimeIdentitySha256  string                      `json:"runtime_identity_sha256"`
	KernelBefore           RuntimeNativeKernelIdentity `json:"kernel_before"`
	KernelAfter            RuntimeNativeKernelIdentity `json:"kernel_after"`
	ImageBeforeSha256      string                      `json:"image_before_sha256"`
	ImageAfterSha256       string                      `json:"image_after_sha256"`
	ProfilesDerivedExactly bool                        `json:"profiles_derived_exactly"`
	ExecutablePins         []Pin                       `json:"executable_pins"`
	DocumentPins           []Pin                       `json:"document_pins"`
	MetadataBytesRead      int64                       `json:"metadata_bytes_read"`
	ExecutableBytesRead    int64                       `json:"executable_bytes_read"`
}

// Both snapshots call the actual protected host methods and re-hash every
// document. A root plan does not make a missing image, tool, profile or changed
// kernel real. Full source provenance remains a separate mandatory component.
func captureNativeQualificationKernelRelease(ctx context.Context, boundary RuntimeResourceBoundary, settings RuntimeResourceBoundarySettings, profileSettings CandidateCoreProfileSettings, host nativeQualificationReleaseHost) (facts nativeQualificationKernelReleaseFacts, resultErr error) {
	facts.Schema = Schema
	facts.Kind = "runtime-native-kernel-release-observation/v1"
	if host == nil || settings.NativeBytes <= 0 || settings.NativeBytes == math.MaxInt64 || settings.DocumentBytes <= 0 || settings.DocumentBytes == math.MaxInt64 || settings.ExecutableBytes <= 0 || settings.ExecutableBytes == math.MaxInt64 || settings.MaximumCorePipeLimit <= 0 {
		return facts, errors.New("native kernel release capture omitted finite bounds")
	}
	identity, err := prospectiveNativeQualificationIdentity(boundary)
	if err != nil {
		return facts, err
	}
	facts.RuntimeIdentitySha256 = identity
	if err := ctx.Err(); err != nil {
		return facts, err
	}
	release := boundary.NativeClass.Release
	remaining := settings.NativeBytes
	readDocument := func(pin Pin) ([]byte, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		maximum := settings.DocumentBytes
		if remaining < maximum {
			maximum = remaining
		}
		if maximum <= 0 {
			return nil, errors.New("native release metadata aggregate exhausted")
		}
		data, err := host.Document(pin, maximum)
		if err != nil || int64(len(data)) > maximum || len(data) == 0 || digestDockerBytes(data) != pin.Sha256 {
			return nil, errors.Join(err, errors.New("native release document absent, oversized, or changed"))
		}
		remaining -= int64(len(data))
		facts.MetadataBytesRead += int64(len(data))
		return data, nil
	}
	for snapshot := 0; snapshot < 2; snapshot++ {
		if err := ctx.Err(); err != nil {
			return facts, err
		}
		kernel, err := host.NativeClassKernel(ctx, settings)
		if err != nil || !validRuntimeNativeKernel(kernel, settings.MaximumCorePipeLimit) {
			return facts, errors.Join(err, errors.New("native kernel observation unsupported or incomplete"))
		}
		if snapshot == 0 {
			facts.KernelBefore = kernel
		} else {
			facts.KernelAfter = kernel
			if kernel != facts.KernelBefore {
				return facts, errors.New("native kernel changed between exact snapshots")
			}
		}
		maximum := settings.DocumentBytes
		if remaining < maximum {
			maximum = remaining
		}
		if maximum <= 0 {
			return facts, errors.New("native image metadata aggregate exhausted")
		}
		imageSettings := settings
		imageSettings.DocumentBytes = maximum
		imageSettings.NativeBytes = maximum
		imageBytes, err := host.NativeClassImage(ctx, boundary, release.EvaluatorImageDigest, imageSettings)
		if err != nil || len(imageBytes) == 0 || int64(len(imageBytes)) > maximum || validateDockerMutationJson(imageBytes, settings.DocumentDepth) != nil {
			return facts, errors.Join(err, errors.New("actual native image readback absent or outside bound"))
		}
		remaining -= int64(len(imageBytes))
		facts.MetadataBytesRead += int64(len(imageBytes))
		var image struct {
			Id           string
			Os           string
			Architecture string
			Config       struct{ OnBuild []string }
		}
		if json.Unmarshal(imageBytes, &image) != nil || image.Id != release.EvaluatorImageDigest || image.Os != "linux" || image.Architecture != "amd64" || len(image.Config.OnBuild) != 0 {
			return facts, errors.New("actual native image identity/platform/hooks differ")
		}
		imageHash := digestDockerBytes(imageBytes)
		if snapshot == 0 {
			facts.ImageBeforeSha256 = imageHash
		} else {
			facts.ImageAfterSha256 = imageHash
			if imageHash != facts.ImageBeforeSha256 {
				return facts, errors.New("immutable native image metadata changed")
			}
		}
		for _, pin := range []Pin{release.Agent, release.Docker, release.Compose, release.Buildx, release.JsonFormatter, release.SubmissionBuilderIdentity} {
			if err := ctx.Err(); err != nil {
				return facts, err
			}
			count, err := host.Executable(ctx, pin, settings.ExecutableBytes)
			if count < 0 || count > settings.ExecutableBytes || count > math.MaxInt64-facts.ExecutableBytesRead {
				return facts, errors.Join(err, errors.New("native executable read counter outside finite bound"))
			}
			facts.ExecutableBytesRead += count
			if err != nil || count == 0 {
				return facts, errors.Join(err, errors.New("native protected executable absent or outside owner bound"))
			}
			if snapshot == 0 {
				facts.ExecutablePins = append(facts.ExecutablePins, pin)
			}
		}
		stock, err := readDocument(release.StockSeccompProfile)
		if err != nil {
			return facts, err
		}
		core, err := readDocument(release.CandidateCoreProfile)
		if err != nil {
			return facts, err
		}
		canonical, err := TransformCandidateCoreProfile(stock, profileSettings)
		if err != nil || !bytes.Equal(canonical, core) {
			return facts, errors.New("actual native profiles are not the exact restrictive derivation")
		}
		facts.ProfilesDerivedExactly = true
		if snapshot == 0 {
			facts.DocumentPins = append(facts.DocumentPins, release.StockSeccompProfile, release.CandidateCoreProfile)
		}
		for _, asset := range release.Assets {
			if _, err := readDocument(asset.Pin); err != nil {
				return facts, err
			}
			if snapshot == 0 {
				facts.DocumentPins = append(facts.DocumentPins, asset.Pin)
			}
		}
	}
	return facts, ctx.Err()
}
