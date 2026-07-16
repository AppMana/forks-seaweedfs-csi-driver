package mountmanager

const winFspCaseSensitiveEnv = "SEAWEEDFS_WINFSP_CASE_SENSITIVE"

func winFspCaseSensitivityArgs(getenv func(string) string) []string {
	if value := getenv(winFspCaseSensitiveEnv); value != "" {
		return []string{"-winfspCaseSensitive=" + value}
	}
	return nil
}
