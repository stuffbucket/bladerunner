// Recovering the disk image file behind an already-mounted volume, by parsing
// `hdiutil info -plist`.
//
// This exists because of how a cartridge actually arrives: the user double
// clicks (or AirDrop auto-opens) a shipped read-only .dmg and macOS mounts it.
// Booting it needs the ORIGINAL FILE, not the mount — a holder re-opens the
// source, converts it to a writable .sparseimage and attaches its own copy, so
// the shipped artifact stays pristine. That file path cannot be recovered from
// the mount: statfs only names the BSD device the DiskImages framework
// synthesized, and the synthetic device has no path back to its backing store.
// `hdiutil info -plist` is the one place macOS publishes the
// image-path <-> device-node correspondence.
//
// The plist decoder itself is shared with `hdiutil attach -plist` (cartridge.go);
// only the shape of the document differs — attach reports one image's
// system-entities at the root, info reports an "images" array of them.

package cartridge

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/stuffbucket/bladerunner/internal/diskarb"
)

// cmdInfo is the hdiutil subcommand listing every attached disk image.
const cmdInfo = "info"

// infoTimeout bounds `hdiutil info`. It only reads framework state (no I/O on
// the images themselves), so it is either fast or wedged.
const infoTimeout = 30 * time.Second

// plist keys consumed from an `hdiutil info -plist` image entry.
const (
	plistKeyImages    = "images"
	plistKeyImagePath = "image-path"
	plistKeyImageType = "image-type"
	// plistKeyWritable carries hdiutil's own spelling of the key.
	plistKeyWritable  = "writeable"
	plistKeyRemovable = "removable"
)

// ErrNoBackingImage reports that no attached disk image owns the device node or
// mountpoint that was asked about — i.e. the volume is real storage (or an
// unrelated filesystem), not a mounted .dmg/.sparseimage. Match it with
// errors.Is.
var ErrNoBackingImage = errors.New("no attached disk image backs that device or mountpoint")

// ErrNoImageRef is returned when a backing-image lookup is asked about an empty
// device node / mountpoint.
var ErrNoImageRef = errors.New("cartridge: no device node or mountpoint given")

// errMalformedInfo reports that `hdiutil info -plist` produced a document this
// build cannot read: the images key is present but not an array, an entry is
// not a dictionary, or an entry omits the image path a lookup has to match on.
//
// It is deliberately DISTINCT from ErrNoBackingImage. Callers act on the
// difference: ErrNoBackingImage is a conclusive "that file is not attached" and
// authorizes an unlink, while this one means the question was never answered
// and must leave the image exactly where it is. Folding the two together is
// what let malformed output authorize deleting the backing store of a live
// mount.
var errMalformedInfo = errors.New("hdiutil info output does not have the shape this build can read")

// ImageBacking is the disk image file behind a mounted volume, together with
// the entity that matched the lookup.
type ImageBacking struct {
	// ImagePath is the backing file on disk, e.g.
	// /Users/me/Downloads/bladerunner-demo.dmg. This is the path a holder must
	// be spawned with to boot the cartridge.
	ImagePath string
	// ImageType is hdiutil's description of the format, e.g. "UDIF read-only
	// compressed (zlib)" for a shipped .dmg or "sparse disk image" for a
	// runnable working copy. It is provenance for messages only; Writable is
	// the field to branch on.
	ImageType string
	// Writable reports whether the image is attached read-write. A shipped
	// UDZO .dmg is false; a .sparseimage is true.
	Writable bool
	// Removable reports whether hdiutil marks the media removable (true for
	// every ordinary disk image).
	Removable bool
	// DevNode is the BSD device node of the entity that matched the lookup,
	// e.g. /dev/disk9s1.
	DevNode string
	// Mountpoint is where the matched entity is mounted. When the lookup
	// matched an unmounted entity (a partition scheme, an APFS container) it
	// falls back to the image's first mounted entity, and is empty when the
	// image carries no mounted volume at all.
	Mountpoint string
}

// attachedImage is one entry of the `images` array in `hdiutil info -plist`.
type attachedImage struct {
	// path is the backing file (image-path).
	path string
	// imageType is hdiutil's format description (image-type).
	imageType string
	// writable is the writeable flag.
	writable bool
	// removable is the removable flag.
	removable bool
	// entities are the device nodes this image produced.
	entities []systemEntity
}

// infoArgs builds the `hdiutil info -plist` argument vector.
func infoArgs() []string { return []string{cmdInfo, flagPlist} }

// backingImageFor is the worker behind the backing-image lookup: it resolves
// ref — a BSD device node in either form ("/dev/disk9s1" as hdiutil prints it,
// or "disk9s1" as DiskArbitration reports it) or a mountpoint
// ("/Volumes/bladerunner-demo") — to the disk image file behind it.
//
// It takes a commandRunner so Detect can withhold one off darwin and so tests
// drive it against captured hdiutil output. It returns an error wrapping
// ErrNoBackingImage when nothing attached matches, which is the normal answer
// for a volume that is not a mounted disk image.
func backingImageFor(ctx context.Context, r commandRunner, ref string) (*ImageBacking, error) {
	if ref == "" {
		return nil, ErrNoImageRef
	}
	images, err := listAttachedImages(ctx, r)
	if err != nil {
		return nil, err
	}
	backing, ok := matchImage(images, ref)
	if !ok {
		return nil, fmt.Errorf("%s: %w", ref, ErrNoBackingImage)
	}
	return backing, nil
}

// attachedImageAt is the REVERSE lookup of backingImageFor: it resolves an
// image FILE path to the attachment macOS is currently serving from it, or
// wraps ErrNoBackingImage when that file is not attached at all.
//
// It exists because the mountpoint of a volume attached by a process that is no
// longer around cannot be derived: under the browsable policy macOS chooses it
// (and suffixes it on a name collision), and the Mount value that recorded it
// died with the process. The image path is the only handle that survives, and
// `hdiutil info -plist` is the one place macOS publishes the
// image-path <-> device-node correspondence.
func attachedImageAt(ctx context.Context, r commandRunner, imagePath string) (*ImageBacking, error) {
	if imagePath == "" {
		return nil, ErrNoImageRef
	}
	images, err := listAttachedImages(ctx, r)
	if err != nil {
		return nil, err
	}
	want := resolvePath(imagePath)
	for _, img := range images {
		// hdiutil reports the path it was handed, which may be spelled
		// differently (/tmp vs /private/tmp) than the one we hold.
		if img.path != imagePath && resolvePath(img.path) != want {
			continue
		}
		return img.backing(img.principalEntity()), nil
	}
	return nil, fmt.Errorf("%s: %w", imagePath, ErrNoBackingImage)
}

// errNothingAttached is the POSITIVE answer that no volume is served from an
// image any more — the only answer that may authorize unlinking it. It is
// returned by attachedDeviceFor and is never produced by a lookup that merely
// failed.
var errNothingAttached = errors.New("cartridge: nothing is attached from that image")

// attachedDeviceFor asks hdiutil which BSD device it currently serves image
// from, so an attachment can be released when the Mount that recorded it is
// gone, incomplete, or was never readable in the first place.
//
// The image FILE is the handle that survives everything else: a mountpoint is
// chosen by macOS and can be occupied by an unrelated volume later, and a device
// node is only known if the attach plist parsed. Three answers, and the caller
// must tell them apart:
//
//   - a device node, which is what to detach;
//   - errNothingAttached, the conclusive "there is nothing to release";
//   - any other error, meaning the state is UNKNOWN — which callers must treat
//     as still attached, never as detached.
func attachedDeviceFor(parent context.Context, r commandRunner, image string) (string, error) {
	if image == "" {
		return "", fmt.Errorf("%w: no image path to identify the attachment by", errMalformedInfo)
	}
	ctx, cancel := context.WithTimeout(parent, infoTimeout)
	defer cancel()

	at, err := attachedImageAt(ctx, r, image)
	switch {
	case errors.Is(err, ErrNoBackingImage):
		return "", errNothingAttached
	case err != nil:
		return "", fmt.Errorf("could not establish what %s is attached as: %w", image, err)
	case at.DevNode == "":
		return "", fmt.Errorf("%w: %s is attached but hdiutil named no device for it", errMalformedInfo, image)
	}
	return at.DevNode, nil
}

// principalEntity is the entity that best represents an image in a lookup that
// matched the IMAGE rather than one of its devices: the first mounted one when
// there is one (that is what a user has to eject), else the first device the
// image produced (an image attached -nomount still has to be detached).
func (img attachedImage) principalEntity() systemEntity {
	for _, e := range img.entities {
		if e.MountPoint != "" {
			return e
		}
	}
	if len(img.entities) > 0 {
		return img.entities[0]
	}
	return systemEntity{}
}

// listAttachedImages runs `hdiutil info -plist` and decodes its images array.
func listAttachedImages(ctx context.Context, r commandRunner) ([]attachedImage, error) {
	out, errOut, err := r.run(ctx, hdiutil, infoArgs()...)
	if err != nil {
		return nil, wrapHdiutil(cmdInfo, err, errOut)
	}
	return parseInfoImages(out)
}

// parseInfoImages decodes the images array from `hdiutil info -plist` stdout.
//
// A well-formed plist that OMITS the images key yields no images and no error:
// "no disk image is attached" is a normal state, and hdiutil omits the key then.
// Everything else that does not decode is an ERROR, never an empty list. This
// parser is the authority behind clearStaleWorkingCopy and confirmDetached, both
// of which unlink or overwrite a file once they read "nothing is attached", so a
// record this build cannot read has to be reported rather than dropped: a
// silently skipped entry is indistinguishable from an absent one, and the absent
// answer is the destructive one.
func parseInfoImages(stdout string) ([]attachedImage, error) {
	root, err := parsePlistRootDict(stdout)
	if err != nil {
		return nil, fmt.Errorf("parse hdiutil %s output: %w", cmdInfo, err)
	}
	value, present := root[plistKeyImages]
	if !present {
		return nil, nil
	}
	raw, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("%w: %s is %T, not an array", errMalformedInfo, plistKeyImages, value)
	}
	images := make([]attachedImage, 0, len(raw))
	for i, item := range raw {
		img, decodeErr := decodeAttachedImage(item)
		if decodeErr != nil {
			return nil, fmt.Errorf("%w: %s[%d]: %w", errMalformedInfo, plistKeyImages, i, decodeErr)
		}
		images = append(images, img)
	}
	return images, nil
}

// decodeAttachedImage converts one entry of the images array into a typed image.
//
// image-path is REQUIRED because it is the key every reverse lookup matches on
// (attachedImageAt): an entry whose path this build cannot read is an
// attachment that would silently fail to match, which reads to the caller as
// "your image is not attached".
func decodeAttachedImage(item any) (attachedImage, error) {
	dict, ok := item.(map[string]any)
	if !ok {
		return attachedImage{}, fmt.Errorf("entry is %T, not a dictionary", item)
	}
	path, err := plistRequiredString(dict, plistKeyImagePath)
	if err != nil {
		return attachedImage{}, err
	}
	entities, err := decodeEntities(dict[plistKeyEntities])
	if err != nil {
		return attachedImage{}, err
	}
	return attachedImage{
		path:      path,
		imageType: plistString(dict, plistKeyImageType),
		writable:  plistBool(dict, plistKeyWritable),
		removable: plistBool(dict, plistKeyRemovable),
		entities:  entities,
	}, nil
}

// decodeEntities converts a decoded system-entities array into typed entities.
// An image that carries no system-entities key at all decodes to no entities,
// which is how an attachment with nothing to address is spelled; a key that is
// present but malformed is an error, for the reason parseInfoImages gives.
//
// parseAttachEntities does the same job for `hdiutil attach -plist`, where the
// array hangs off the ROOT dict; here it hangs off each image, so the walk
// starts from an already-decoded value rather than from stdout.
func decodeEntities(raw any) ([]systemEntity, error) {
	if raw == nil {
		return nil, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("%s is %T, not an array", plistKeyEntities, raw)
	}
	entities := make([]systemEntity, 0, len(arr))
	for i, item := range arr {
		dict, ok := item.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s[%d] is %T, not a dictionary", plistKeyEntities, i, item)
		}
		entities = append(entities, systemEntity{
			DevEntry:   plistString(dict, plistKeyDevEntry),
			MountPoint: plistString(dict, plistKeyMountPoint),
			VolumeKind: plistString(dict, plistKeyVolumeKind),
		})
	}
	return entities, nil
}

// plistRequiredString reads a key a lookup cannot proceed without: a missing
// key and a wrong-typed value are both reported rather than read as "".
func plistRequiredString(dict map[string]any, key string) (string, error) {
	value, present := dict[key]
	if !present {
		return "", fmt.Errorf("no %s key", key)
	}
	s, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%s is %T, not a string", key, value)
	}
	return s, nil
}

// plistBool reads a boolean-valued key from a decoded plist dict, yielding
// false for a missing key or a non-boolean value.
func plistBool(dict map[string]any, key string) bool {
	b, _ := dict[key].(bool)
	return b
}

// matchImage finds the attached image that owns ref and describes it.
//
// Both addressing forms are tried against every entity of every image, because
// the caller may hold either: DiskArbitration hands out BSD names, while a
// Finder-mounted volume is usually only known by its /Volumes path. Matching
// the WHOLE-disk entity counts too, so "/dev/disk9" resolves to the same image
// as its mounted "/dev/disk9s1" slice.
//
// diskarb.DevPath is what tells the two forms apart: it renders every spelling
// of a device (bare "disk9s1", block "/dev/disk9s1", raw "/dev/rdisk9s1") in
// the one form hdiutil records, and answers "" for a mountpoint — so the empty
// answer IS the "ref is not a device" test. The rule lives there, once.
func matchImage(images []attachedImage, ref string) (*ImageBacking, bool) {
	dev := diskarb.DevPath(ref)
	want := resolvePath(ref)
	for _, img := range images {
		for _, e := range img.entities {
			if !entityMatches(e, dev, ref, want) {
				continue
			}
			return img.backing(e), true
		}
	}
	return nil, false
}

// entityMatches reports whether entity e is the one ref names, by BSD device
// node (dev, empty when ref is not a device reference) or by mountpoint (ref
// literally, or want as its symlink-resolved form — hdiutil reports
// /private/tmp/x for a /tmp/x mount).
func entityMatches(e systemEntity, dev, ref, want string) bool {
	if dev != "" && e.DevEntry == dev {
		return true
	}
	if e.MountPoint == "" {
		return false
	}
	return e.MountPoint == ref || resolvePath(e.MountPoint) == want
}

// backing describes img as the answer to a lookup that matched entity e.
func (img attachedImage) backing(e systemEntity) *ImageBacking {
	mountpoint := e.MountPoint
	if mountpoint == "" {
		mountpoint = img.firstMountpoint()
	}
	return &ImageBacking{
		ImagePath:  img.path,
		ImageType:  img.imageType,
		Writable:   img.writable,
		Removable:  img.removable,
		DevNode:    e.DevEntry,
		Mountpoint: mountpoint,
	}
}

// firstMountpoint returns where this image's first mounted entity is mounted,
// or "" when it was attached with -nomount (or has no mountable volume).
func (img attachedImage) firstMountpoint() string {
	for _, e := range img.entities {
		if e.MountPoint != "" {
			return e.MountPoint
		}
	}
	return ""
}

// The BSD device-name rule — which spellings name a device, and what the
// canonical /dev form of each one is — belongs to internal/diskarb. This file
// carried its own copy and the copy disagreed on the raw-device spelling;
// matchImage now calls diskarb.DevPath.
