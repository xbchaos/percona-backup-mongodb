package sharded

func (c *Cluster) DistributedTrxSnapshot() {
	c.DistributedTransactions(NewSnapshot(c), "testtrx")
}

func (c *Cluster) DistributedTrxPhysical() {
	c.DistributedTransactionsPhys(NewPhysical(c), "testtrx")
}
