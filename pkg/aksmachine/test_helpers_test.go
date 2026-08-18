package aksmachine

func testGoal(kubernetesVersion, settingsVersion string) GoalState {
	return GoalState{
		KubernetesVersion: kubernetesVersion,
		SettingsVersion:   settingsVersion,
		MaxPods:           ptr(110),
		KubeletConfig: KubeletConfig{
			ImageGCHighThreshold: ptr(85),
			ImageGCLowThreshold:  ptr(80),
		},
	}
}
