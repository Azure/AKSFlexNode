package aksmachine

func testGoal(kubernetesVersion, settingsVersion string) GoalState {
	return GoalState{
		KubernetesVersion: kubernetesVersion,
		SettingsVersion:   settingsVersion,
		MaxPods:           110,
		KubeletConfig: KubeletConfig{
			ImageGCHighThreshold: 85,
			ImageGCLowThreshold:  80,
		},
	}
}
