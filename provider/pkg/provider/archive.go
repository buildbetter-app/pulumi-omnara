package provider

import (
	"archive/zip"
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/archive"
)

// skillNameRE is Omnara's rule for skill names: lowercase letters and digits
// in hyphen-separated segments, up to 64 characters.
var skillNameRE = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// skill is the content of a skill archive.
type skill struct {
	name  string
	files map[string][]byte
	// prefix is prepended to every file to give the zip one top-level
	// directory; it is empty when the archive already has one.
	prefix string
}

// readSkill reads a Pulumi archive that holds SKILL.md at its root, or one
// directory that contains SKILL.md.
func readSkill(a *archive.Archive) (*skill, error) {
	if a == nil || !a.HasContents() {
		return nil, errors.New("archive has no contents")
	}
	r, err := a.Open()
	if err != nil {
		return nil, fmt.Errorf("open archive: %w", err)
	}
	defer r.Close()
	files := map[string][]byte{}
	for {
		name, blob, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read archive: %w", err)
		}
		b, err := io.ReadAll(blob)
		blob.Close()
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", name, err)
		}
		clean := path.Clean(strings.TrimPrefix(strings.ReplaceAll(name, "\\", "/"), "./"))
		if strings.HasPrefix(clean, "/") || clean == ".." || strings.HasPrefix(clean, "../") {
			return nil, fmt.Errorf("archive entry %q is outside the archive root", name)
		}
		if ignoredSkillFile(clean) {
			continue
		}
		files[clean] = b
	}

	sk := &skill{files: files}
	md, atRoot := files["SKILL.md"]
	if !atRoot {
		top := singleTopDir(files)
		if md = files[top+"/SKILL.md"]; top == "" || md == nil {
			return nil, errors.New("archive must hold SKILL.md at its root, or one directory that contains SKILL.md")
		}
	}
	if sk.name, err = skillName(md); err != nil {
		return nil, err
	}
	if atRoot {
		sk.prefix = sk.name + "/"
	}
	return sk, nil
}

// skillArchiveName returns the skill name an archive declares.
func skillArchiveName(a *archive.Archive) (string, error) {
	sk, err := readSkill(a)
	if err != nil {
		return "", err
	}
	return sk.name, nil
}

// zipSkill packs a Pulumi archive into the zip layout Omnara's skill upload
// expects: one top-level directory that contains SKILL.md.
func zipSkill(a *archive.Archive) ([]byte, error) {
	sk, err := readSkill(a)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(sk.files))
	for n := range sk.files {
		names = append(names, n)
	}
	sort.Strings(names)
	// A fixed timestamp keeps the upload byte-for-byte reproducible.
	epoch := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, n := range names {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: sk.prefix + n, Method: zip.Deflate, Modified: epoch})
		if err != nil {
			return nil, err
		}
		if _, err := w.Write(sk.files[n]); err != nil {
			return nil, err
		}
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func ignoredSkillFile(name string) bool {
	base := path.Base(name)
	return strings.HasPrefix(name, "__MACOSX/") || base == ".DS_Store" || strings.HasPrefix(base, "._")
}

func singleTopDir(files map[string][]byte) string {
	top := ""
	for n := range files {
		i := strings.Index(n, "/")
		if i < 0 {
			return ""
		}
		if top == "" {
			top = n[:i]
		} else if top != n[:i] {
			return ""
		}
	}
	return top
}

// skillName reads `name:` from SKILL.md's YAML frontmatter.
func skillName(md []byte) (string, error) {
	sc := bufio.NewScanner(bytes.NewReader(md))
	if !sc.Scan() || strings.TrimSpace(sc.Text()) != "---" {
		return "", errors.New("SKILL.md must start with YAML frontmatter")
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.TrimSpace(line) == "---" {
			break
		}
		if v, ok := strings.CutPrefix(line, "name:"); ok {
			v = strings.Trim(strings.TrimSpace(v), `"'`)
			if len(v) > 64 || !skillNameRE.MatchString(v) {
				return "", fmt.Errorf("SKILL.md name %q must be lowercase letters and digits in hyphen-separated segments, up to 64 characters", v)
			}
			return v, nil
		}
	}
	return "", errors.New("SKILL.md frontmatter has no name")
}
