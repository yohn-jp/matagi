package desktop

func shouldHideOwnedConsole(current uint32, attached []uint32) bool {
	return current != 0 && len(attached) == 1 && attached[0] == current
}
