package hubclient

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type workspaceGitRepository struct {
	Path string `json:"path"`
	Name string `json:"name"`
	ID   string `json:"id"`
}
type workspaceGitRepositories struct {
	Repositories []workspaceGitRepository `json:"repositories"`
	Truncated    bool                     `json:"truncated"`
}

func gitRepositoryID(root string) string {
	return gitReviewHash(filepath.ToSlash(filepath.Clean(root)))
}

// Every directory component and the opened handle must remain under the saved
// project. Git files used by worktrees/submodules may point only inside it too.
func gitRepositoryDirectory(root *os.Root, rootHandle *os.File, workspace, relative string) error {
	if relative == "." {
		relative = ""
	}
	if filepath.IsAbs(relative) || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return errWorkspaceFiles
	}
	current := ""
	for _, part := range strings.Split(filepath.ToSlash(relative), "/") {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := root.Lstat(current)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errWorkspaceFiles
		}
	}
	name := relative
	if name == "" {
		name = "."
	}
	handle, err := root.Open(name)
	if err != nil {
		return errWorkspaceFiles
	}
	defer handle.Close()
	return workspaceVerifyHandle(rootHandle, handle, workspace, filepath.ToSlash(relative), false)
}

func gitRepositoryMetadata(root *os.Root, rootHandle *os.File, workspace, relative string) error {
	marker := filepath.Join(relative, ".git")
	info, err := root.Lstat(marker)
	if err != nil || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("git_not_repository: 当前目录不是可访问的 Git 仓库")
	}
	gitDirectory := marker
	if info.Mode().IsRegular() {
		if info.Size() > 4096 {
			return errWorkspaceFiles
		}
		file, e := workspaceRootReadFile(root, marker)
		if e != nil {
			return errWorkspaceFiles
		}
		defer file.Close()
		if workspaceVerifyHandle(rootHandle, file, workspace, filepath.ToSlash(marker), true) != nil {
			return errWorkspaceFiles
		}
		data, e := io.ReadAll(io.LimitReader(file, 4097))
		if e != nil || len(data) > 4096 {
			return errWorkspaceFiles
		}
		value, ok := strings.CutPrefix(strings.TrimSpace(string(data)), "gitdir: ")
		if !ok || strings.ContainsAny(value, "\x00\r\n") {
			return errWorkspaceFiles
		}
		candidate := value
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(workspace, relative, candidate)
		}
		gitDirectory, e = filepath.Rel(workspace, filepath.Clean(candidate))
		if e != nil {
			return errWorkspaceFiles
		}
	} else if !info.IsDir() {
		return errWorkspaceFiles
	}
	if err = gitRepositoryDirectory(root, rootHandle, workspace, gitDirectory); err != nil {
		return err
	}
	commonFile := filepath.Join(gitDirectory, "commondir")
	if commonInfo, e := root.Lstat(commonFile); e == nil {
		if !commonInfo.Mode().IsRegular() || commonInfo.Size() > 4096 {
			return errWorkspaceFiles
		}
		file, e := workspaceRootReadFile(root, commonFile)
		if e != nil {
			return errWorkspaceFiles
		}
		defer file.Close()
		if workspaceVerifyHandle(rootHandle, file, workspace, filepath.ToSlash(commonFile), true) != nil {
			return errWorkspaceFiles
		}
		data, e := io.ReadAll(io.LimitReader(file, 4097))
		if e != nil || len(data) > 4096 {
			return errWorkspaceFiles
		}
		value := strings.TrimSpace(string(data))
		if value == "" || strings.ContainsAny(value, "\x00\r\n") {
			return errWorkspaceFiles
		}
		candidate := value
		if !filepath.IsAbs(candidate) {
			candidate = filepath.Join(workspace, gitDirectory, candidate)
		}
		common, e := filepath.Rel(workspace, filepath.Clean(candidate))
		if e != nil {
			return errWorkspaceFiles
		}
		if e = gitRepositoryDirectory(root, rootHandle, workspace, common); e != nil {
			return e
		}
	} else if !os.IsNotExist(e) {
		return errWorkspaceFiles
	}
	return nil
}

func workspaceGitRepositoryRoot(ctx context.Context, workspace string, rootHandle *os.File, relative string) (string, workspaceGitRepository, error) {
	repository := workspaceGitRepository{Path: relative}
	if relative != "" && !gitSafePath(relative) {
		return "", repository, errors.New("git_repository_path_invalid")
	}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return "", repository, err
	}
	defer root.Close()
	if err = gitRepositoryDirectory(root, rootHandle, workspace, filepath.FromSlash(relative)); err != nil {
		return "", repository, err
	}
	if err = gitRepositoryMetadata(root, rootHandle, workspace, filepath.FromSlash(relative)); err != nil {
		return "", repository, err
	}
	candidate := filepath.Join(workspace, filepath.FromSlash(relative))
	top, err := workspaceGitRun(ctx, candidate, "git", "rev-parse", "--show-toplevel")
	if err != nil || !workspaceSamePath(candidate, strings.TrimSpace(top)) {
		return "", repository, errors.New("git_root_mismatch: 仓库根目录已变化")
	}
	repository.Name = filepath.Base(candidate)
	repository.ID = gitRepositoryID(candidate)
	return candidate, repository, nil
}

func workspaceDiscoverGitRepositories(ctx context.Context, workspace string, rootHandle *os.File) (workspaceGitRepositories, error) {
	result := workspaceGitRepositories{Repositories: []workspaceGitRepository{}}
	root, err := os.OpenRoot(workspace)
	if err != nil {
		return result, err
	}
	defer root.Close()
	type directory struct {
		path  string
		depth int
	}
	queue := []directory{{"", 0}}
	scanned := 0
	skipped := map[string]bool{".git": true, "node_modules": true, "vendor": true, ".next": true, ".gradle": true, ".expo": true, ".cache": true, "__pycache__": true}
	for len(queue) > 0 {
		if err = ctx.Err(); err != nil {
			return result, err
		}
		item := queue[0]
		queue = queue[1:]
		if _, repository, e := workspaceGitRepositoryRoot(ctx, workspace, rootHandle, item.path); e == nil {
			if len(result.Repositories) >= 32 {
				result.Truncated = true
				break
			}
			result.Repositories = append(result.Repositories, repository)
		} else if _, markerErr := root.Lstat(filepath.Join(filepath.FromSlash(item.path), ".git")); markerErr == nil || !os.IsNotExist(markerErr) {
			result.Truncated = true
		}
		if e := gitRepositoryDirectory(root, rootHandle, workspace, filepath.FromSlash(item.path)); e != nil {
			result.Truncated = true
			continue
		}
		name := item.path
		if name == "" {
			name = "."
		}
		file, e := root.Open(filepath.FromSlash(name))
		if e != nil {
			result.Truncated = true
			continue
		}
		if workspaceVerifyHandle(rootHandle, file, workspace, item.path, false) != nil {
			file.Close()
			result.Truncated = true
			continue
		}
		for {
			entries, readErr := file.ReadDir(64)
			for _, entry := range entries {
				scanned++
				if scanned > 20000 {
					result.Truncated = true
					break
				}
				if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || skipped[entry.Name()] {
					continue
				}
				child := entry.Name()
				if item.path != "" {
					child = item.path + "/" + child
				}
				if !gitSafePath(child) {
					continue
				}
				if item.depth >= 8 || len(queue) >= 4096 {
					result.Truncated = true
					continue
				}
				queue = append(queue, directory{child, item.depth + 1})
			}
			if readErr != nil || scanned > 20000 {
				if readErr != nil && !errors.Is(readErr, io.EOF) {
					result.Truncated = true
				}
				break
			}
		}
		file.Close()
		if scanned > 20000 {
			break
		}
	}
	sort.Slice(result.Repositories, func(i, j int) bool { return result.Repositories[i].Path < result.Repositories[j].Path })
	return result, nil
}

// A parent review never owns files below another repository boundary. Metadata
// is checked through os.Root; inaccessible markers are excluded conservatively.
func gitCrossesRepository(root, path string) bool {
	opened, err := os.OpenRoot(root)
	if err != nil {
		return true
	}
	defer opened.Close()
	current := ""
	for _, part := range strings.Split(strings.TrimSuffix(path, "/"), "/") {
		current = filepath.Join(current, part)
		info, e := opened.Lstat(current)
		if e != nil {
			return !os.IsNotExist(e)
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return false
		}
		if !info.IsDir() {
			return false
		}
		if _, e = opened.Lstat(filepath.Join(current, ".git")); e == nil || !os.IsNotExist(e) {
			return true
		}
	}
	return false
}
