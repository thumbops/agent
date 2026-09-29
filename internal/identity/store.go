package identity

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// SecretStore keeps the state in a Secret of the agent's namespace. The
// Secret is created empty by the manifest: the agent only needs get and
// update on that one Secret, never create.
type SecretStore struct {
	Client    kubernetes.Interface
	Namespace string
	Name      string
}

func (s SecretStore) Load(ctx context.Context) (*State, error) {
	sec, err := s.Client.CoreV1().Secrets(s.Namespace).Get(ctx, s.Name, metav1.GetOptions{})
	if err != nil {
		return nil, s.wrap(err)
	}
	st, err := decode(sec.Data)
	if err != nil {
		return nil, fmt.Errorf("Secret %s/%s: %w", s.Namespace, s.Name, err)
	}
	return st, nil
}

// Save replaces the Secret data in one update, guarded by its resourceVersion.
func (s SecretStore) Save(ctx context.Context, st *State) error {
	data, err := st.encode()
	if err != nil {
		return err
	}
	secrets := s.Client.CoreV1().Secrets(s.Namespace)
	sec, err := secrets.Get(ctx, s.Name, metav1.GetOptions{})
	if err != nil {
		return s.wrap(err)
	}
	sec.Data = data
	if _, err := secrets.Update(ctx, sec, metav1.UpdateOptions{}); err != nil {
		return s.wrap(err)
	}
	return nil
}

func (s SecretStore) wrap(err error) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("Secret %s/%s not found: it is created empty by the Helm chart (or deploy/agent.yaml)", s.Namespace, s.Name)
	}
	return fmt.Errorf("Secret %s/%s: %w", s.Namespace, s.Name, err)
}

// FileStore keeps the state in a directory, for development outside the cluster.
type FileStore struct {
	Dir string
}

func (s FileStore) Load(context.Context) (*State, error) {
	data := map[string][]byte{}
	for _, name := range []string{keyKey, certKey, caKey, clusterIDKey, tokenHashKey} {
		b, err := os.ReadFile(filepath.Join(s.Dir, name))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		data[name] = b
	}
	return decode(data)
}

// Save writes every file atomically; the certificate goes last, because
// its presence is what makes the state exist.
func (s FileStore) Save(_ context.Context, st *State) error {
	data, err := st.encode()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.Dir, 0o700); err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(s.Dir, certKey)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, name := range []string{keyKey, caKey, clusterIDKey, tokenHashKey, certKey} {
		if b, ok := data[name]; ok {
			if err := writeAtomic(filepath.Join(s.Dir, name), b); err != nil {
				return err
			}
		}
	}
	return nil
}

func writeAtomic(path string, data []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
