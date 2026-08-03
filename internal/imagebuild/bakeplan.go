package imagebuild

import (
	"fmt"
	"path/filepath"
	"strconv"
)

// Image tooling the bake shells out to. qemu-img is not reimplemented: it is
// the reference implementation of the qcow2 format and the shell build already
// depended on it, so this is not a new dependency.
const (
	qemuImgTool = "qemu-img"
	// qcow2Format is the output format, named once so the argv builders and
	// any future reader agree.
	qcow2Format = "qcow2"
	// gibSuffix is what qemu-img resize expects after a size in GiB.
	gibSuffix = "G"
	// partialSuffix names the in-progress output. The result is renamed into
	// place only once it is complete, so a failed bake never leaves a
	// half-written image at the path a later step reads.
	partialSuffix = ".partial"
)

// BakePhase names one stage of a bake, in the order they run.
type BakePhase string

const (
	// PhaseFetch acquires the reviewed base image.
	PhaseFetch BakePhase = "fetch"
	// PhaseResize grows the working image to the requested size.
	PhaseResize BakePhase = "resize"
	// PhaseCustomize applies the recipe.
	PhaseCustomize BakePhase = "customize"
	// PhaseCompress writes the compressed output.
	PhaseCompress BakePhase = "compress"
	// PhasePublish renames the finished image into place.
	PhasePublish BakePhase = "publish"
)

// BakePlan is the ordered work of one bake, as data.
//
// It is data for the same reason Recipe.Steps() is: the ordering carries real
// dependencies that fail silently rather than loudly when inverted — resizing
// after customizing grows an image whose filesystem does not use the space, and
// compressing before customizing throws the work away. A plan can be asserted
// without root, an nbd device, or a network.
type BakePlan struct {
	// Release is the reviewed base to start from.
	Release Release
	// BasePath is where the base image is fetched to and then modified.
	BasePath string
	// SizeGiB is what the working image is grown to before customizing.
	SizeGiB int
	// Recipe is what gets applied.
	Recipe Recipe
	// PartialPath is the compressed output before it is named.
	PartialPath string
	// OutputPath is the finished image.
	OutputPath string
}

// NewBakePlan resolves a bake into its ordered work.
//
// workDir holds the base image and the partial output; both are large, so they
// are deliberately kept together and away from the destination until the last
// step.
func NewBakePlan(r Release, recipe Recipe, workDir, outputPath string, sizeGiB int) (BakePlan, error) {
	if outputPath == "" {
		return BakePlan{}, fmt.Errorf("no output path for the %s bake", r.Arch)
	}
	if sizeGiB <= 0 {
		return BakePlan{}, fmt.Errorf("working image size must be positive, got %d", sizeGiB)
	}
	return BakePlan{
		Release:     r,
		BasePath:    filepath.Join(workDir, r.FileName()),
		SizeGiB:     sizeGiB,
		Recipe:      recipe,
		PartialPath: outputPath + partialSuffix,
		OutputPath:  outputPath,
	}, nil
}

// Phases is the order a bake runs in.
//
// Returned rather than declared at each call site so the sequence has one
// owner, and so a test can assert the order without performing it.
func (p BakePlan) Phases() []BakePhase {
	return []BakePhase{PhaseFetch, PhaseResize, PhaseCustomize, PhaseCompress, PhasePublish}
}

// ResizeArgs is the command that grows the working image before the recipe is
// applied. Growing afterwards would leave a filesystem that does not use the
// new space.
func (p BakePlan) ResizeArgs() []string {
	return []string{qemuImgTool, "resize", p.BasePath, strconv.Itoa(p.SizeGiB) + gibSuffix}
}

// CompressArgs is the command that writes the compressed output.
//
// -c is what makes the published image a fraction of the working size. It is
// worth having because the free space was zeroed during customize, which is the
// step that lets the compressor discard it.
func (p BakePlan) CompressArgs() []string {
	return []string{qemuImgTool, "convert", "-O", qcow2Format, "-c", p.BasePath, p.PartialPath}
}
