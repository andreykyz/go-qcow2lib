package qcow2

import (
	"unsafe"
)

/*
Copyright (c) 2023 Yunpeng Deng
Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:
The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.
THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

func l2_load(bs *BlockDriverState, offset uint64, l2Offset uint64) (unsafe.Pointer, error) {
	s := bs.opaque.(*BDRVQcow2State)
	startOfSlice := l2_entry_size(s) * (offset_to_l2_index(s, offset) - offset_to_l2_slice_index(s, offset))
	return qcow2_cache_get(bs, s.L2TableCache, l2Offset+startOfSlice)
}

func qcow2_write_l1_entry(bs *BlockDriverState, l1Index uint32) error {
	s := bs.opaque.(*BDRVQcow2State)
	var err error
	var l1StartIndex uint64

	//at least write a RequestAlignment if RequestAlignment < ClusterSize
	var bufsize uint64 = (max(L1E_SIZE,
		min(uint64(bs.current.bs.RequestAlignment), uint64(s.ClusterSize))))
	nentries := int(bufsize) / int(L1E_SIZE)

	buf := make([]uint64, nentries)
	l1StartIndex = align_down(uint64(l1Index), uint64(nentries))

	for i := uint64(0); i < min(uint64(nentries), uint64(s.L1Size)-l1StartIndex); i++ {
		buf[i] = cpu_to_be64(s.L1Table[l1StartIndex+i])
	}

	if err = bdrv_pwrite(bs.current, s.L1TableOffset+L1E_SIZE*l1StartIndex,
		unsafe.Pointer(&buf[0]), bufsize); err != nil {
		return err
	}

	return nil
}

func l2_allocate(bs *BlockDriverState, l1Index uint32) error {
	s := bs.opaque.(*BDRVQcow2State)
	var oldL2Offset uint64
	var l2Slice unsafe.Pointer
	var slice, sliceSize2, nSlices uint
	var l2Offset uint64
	var err error

	oldL2Offset = s.L1Table[l1Index]

	/* allocate a new l2 entry */
	if l2Offset, err = qcow2_alloc_clusters(bs, uint64(s.L2Size)*l2_entry_size(s)); err != nil {
		goto fail
	}
	/* If we're allocating the table at offset 0 then something is wrong */
	if l2Offset == 0 {
		err = Err_L2Alloc
		goto fail
	}
	if err = qcow2_cache_flush(bs, s.RefcountBlockCache); err != nil {
		goto fail
	}

	/* allocate a new entry in the l2 cache */
	sliceSize2 = uint(s.L2SliceSize) * uint(l2_entry_size(s))
	nSlices = uint(s.ClusterSize) / sliceSize2

	for slice = 0; slice < nSlices; slice++ {
		if l2Slice, err = qcow2_cache_get_empty(bs, s.L2TableCache,
			uint64(l2Offset)+uint64(slice*sliceSize2)); err != nil {
			goto fail
		}
		if oldL2Offset&L1E_OFFSET_MASK == 0 {
			/* if there was no old l2 table, clear the new slice */
			memset(l2Slice, int(sliceSize2))
		} else {
			var oldSlice unsafe.Pointer
			oldL2SliceOffset := (oldL2Offset & L1E_OFFSET_MASK) + uint64(slice*sliceSize2)

			/* if there was an old l2 table, read a slice from the disk */
			if oldSlice, err = qcow2_cache_get(bs, s.L2TableCache, oldL2SliceOffset); err != nil {
				goto fail
			}

			memcpy(l2Slice, oldSlice, uint64(sliceSize2))
			qcow2_cache_put(s.L2TableCache, oldSlice)
		}
		qcow2_cache_entry_mark_dirty(s.L2TableCache, l2Slice)
		qcow2_cache_put(s.L2TableCache, l2Slice)
	}

	if err = qcow2_cache_flush(bs, s.L2TableCache); err != nil {
		goto fail
	}
	/* update the L1 entry */
	s.L1Table[l1Index] = l2Offset | uint64(QCOW_OFLAG_COPIED)
	if err = qcow2_write_l1_entry(bs, l1Index); err != nil {
		goto fail
	}
	return nil

fail:
	if l2Slice != nil {
		qcow2_cache_put(s.L2TableCache, l2Slice)
	}
	s.L1Table[l1Index] = oldL2Offset
	if l2Offset > 0 {
		qcow2_free_clusters(bs, l2Offset, uint64(s.L2Size)*l2_entry_size(s))
	}
	return err
}

func get_cluster_table(bs *BlockDriverState, offset uint64) (unsafe.Pointer, uint32, error) {
	s := bs.opaque.(*BDRVQcow2State)
	var l2Index uint32
	var l1Index, l2Offset uint64
	var l2Slice unsafe.Pointer
	var err error

	/* seek to the l2 offset in the l1 table */
	l1Index = offset_to_l1_index(s, offset)
	if uint32(l1Index) >= s.L1Size {
		return nil, 0, Err_IdxOutOfRange
	}
	l2Offset = s.L1Table[l1Index] & L1E_OFFSET_MASK
	if offset_into_cluster(s, l2Offset) > 0 {
		return nil, 0, ERR_EIO
	}

	if s.L1Table[l1Index]&QCOW_OFLAG_COPIED == 0 {
		/* First allocate a new L2 table (and do COW if needed) */
		if err = l2_allocate(bs, uint32(l1Index)); err != nil {
			return nil, 0, err
		}

		/* Then decrease the refcount of the old table */
		if l2Offset > 0 {
			qcow2_free_clusters(bs, l2Offset, uint64(s.L2Size)*l2_entry_size(s))
		}

		/* Get the offset of the newly-allocated l2 table */
		l2Offset = s.L1Table[l1Index] & L1E_OFFSET_MASK
	}

	/* load the l2 slice in memory */
	if l2Slice, err = l2_load(bs, offset, l2Offset); err != nil {
		return nil, 0, err
	}
	/* find the cluster offset for the given disk offset */
	l2Index = uint32(offset_to_l2_slice_index(s, offset))

	return l2Slice, l2Index, nil
}

func qcow2_get_host_offset(bs *BlockDriverState, offset uint64, bytes *uint32,
	hostOffset *uint64, scType *QCow2SubclusterType) error {
	s := bs.opaque.(*BDRVQcow2State)
	var l2Index, scIndex uint64
	var l1Index, l2Offset, l2Entry, l2Bitmap uint64
	var l2Slice unsafe.Pointer
	var sc uint64
	var offsetInCluster uint32
	var bytesAvailable, bytesNeeded, nbClusters uint64
	var tmpType QCow2SubclusterType
	var err error

	offsetInCluster = uint32(offset_into_cluster(s, offset))
	bytesNeeded = uint64(*bytes) + uint64(offsetInCluster)

	/* compute how many bytes there are between the start of the cluster
	 * containing offset and the end of the l2 slice that contains
	 * the entry pointing to it */
	bytesAvailable = uint64(s.L2SliceSize-int(offset_to_l2_slice_index(s, offset))) << uint64(s.ClusterBits)
	if bytesNeeded > bytesAvailable {
		bytesNeeded = bytesAvailable
	}

	*hostOffset = 0

	/* seek to the l2 offset in the l1 table */
	l1Index = offset_to_l1_index(s, offset)
	if l1Index >= uint64(s.L1Size) {
		tmpType = QCOW2_SUBCLUSTER_UNALLOCATED_PLAIN
		goto out
	}

	l2Offset = s.L1Table[l1Index] & L1E_OFFSET_MASK
	if l2Offset == 0 {
		tmpType = QCOW2_SUBCLUSTER_UNALLOCATED_PLAIN
		goto out
	}

	if offset_into_cluster(s, l2Offset) > 0 {
		return ERR_EIO
	}

	/* load the l2 slice in memory */
	if l2Slice, err = l2_load(bs, offset, l2Offset); err != nil {
		return err
	}

	/* find the cluster offset for the given disk offset */
	l2Index = offset_to_l2_slice_index(s, offset)
	scIndex = offset_to_sc_index(s, offset)
	l2Entry = get_l2_entry(s, l2Slice, uint32(l2Index))
	l2Bitmap = get_l2_bitmap(s, l2Slice, uint32(l2Index))
	nbClusters = size_to_clusters(s, bytesNeeded)

	tmpType = qcow2_get_subcluster_type(bs, l2Entry, l2Bitmap, scIndex)
	if s.QcowVersion < 3 && (tmpType == QCOW2_SUBCLUSTER_ZERO_PLAIN ||
		tmpType == QCOW2_SUBCLUSTER_ZERO_ALLOC) {
		err = ERR_EIO
		goto fail
	}

	switch tmpType {
	case QCOW2_SUBCLUSTER_INVALID:
		//do nothing
	case QCOW2_SUBCLUSTER_COMPRESSED:
		// *hostOffset = l2Entry & L2E_COMPRESSED_OFFSET_SIZE_MASK;
		//do nothing
	case QCOW2_SUBCLUSTER_ZERO_PLAIN, QCOW2_SUBCLUSTER_UNALLOCATED_PLAIN:
		//do nothing
	case QCOW2_SUBCLUSTER_ZERO_ALLOC, QCOW2_SUBCLUSTER_NORMAL, QCOW2_SUBCLUSTER_UNALLOCATED_ALLOC:

		hostClusterOffset := l2Entry & L2E_OFFSET_MASK
		*hostOffset = hostClusterOffset + uint64(offsetInCluster)
		if offset_into_cluster(s, hostClusterOffset) > 0 {
			err = ERR_EIO
			goto fail
		}

	default:
		panic("unexpected")
	}

	if sc, err = count_contiguous_subclusters(bs, nbClusters, scIndex, l2Slice, &l2Index); err != nil {
		goto fail
	}

	qcow2_cache_put(s.L2TableCache, l2Slice)
	bytesAvailable = (sc + scIndex) << s.SubclusterBits

out:
	if bytesAvailable > bytesNeeded {
		bytesAvailable = bytesNeeded
	}
	*bytes = uint32(bytesAvailable) - offsetInCluster
	*scType = tmpType
	return nil
fail:
	qcow2_cache_put(s.L2TableCache, l2Slice)
	return err
}

func qcow2_alloc_cluster_link_l2(bs *BlockDriverState, m *QCowL2Meta) error {

	s := bs.opaque.(*BDRVQcow2State)
	var i, j, l2Index uint32
	var err error
	var l2Slice unsafe.Pointer
	var oldCluster []uint64
	clusterOffset := m.AllocOffset

	oldCluster = make([]uint64, m.NbClusters)

	//copy content of unmodified sectors
	if err = perform_cow(bs, m); err != nil {
		goto err
	}

	/* Update L2 table. */
	qcow2_cache_set_dependency(bs, s.L2TableCache,
		s.RefcountBlockCache)

	if l2Slice, l2Index, err = get_cluster_table(bs, m.Offset); err != nil {
		goto err
	}
	qcow2_cache_entry_mark_dirty(s.L2TableCache, l2Slice)

	for i = 0; i < uint32(m.NbClusters); i++ {

		offset := clusterOffset + uint64(i<<s.ClusterBits)
		if get_l2_entry(s, l2Slice, l2Index+i) != 0 {
			oldCluster[j] = get_l2_entry(s, l2Slice, l2Index+i)
			j++
		}
		set_l2_entry(s, l2Slice, l2Index+i, offset|QCOW_OFLAG_COPIED)

		/* Update bitmap with the subclusters that were just written */
		if has_subclusters(s) {
			l2Bitmap := get_l2_bitmap(s, l2Slice, l2Index+i)

			writtenFrom := m.CowStart.Offset
			writtenTo := m.CowEnd.Offset + m.CowEnd.NbBytes
			var firstSc, lastSc uint32
			/* Narrow written_from and written_to down to the current cluster */
			writtenFrom = max(writtenFrom, uint64(i<<s.ClusterBits))
			writtenTo = min(writtenTo, uint64((i+1)<<s.ClusterBits))
			firstSc = uint32(offset_to_sc_index(s, writtenFrom))
			lastSc = uint32(offset_to_sc_index(s, writtenTo-1))
			l2Bitmap |= uint64(qcow_oflag_sub_alloc_range(firstSc, lastSc+1))
			l2Bitmap &= uint64(^qcow_oflag_sub_zero_range(firstSc, lastSc+1))
			set_l2_bitmap(s, l2Slice, l2Index+i, l2Bitmap)
		}
	}

	qcow2_cache_put(s.L2TableCache, l2Slice)

	if !m.KeepOldClusters && j != 0 {
		for i = 0; i < j; i++ {
			qcow2_free_any_cluster(bs, oldCluster[i])
		}
	}
	err = nil
err:
	return err
}

func qcow2_alloc_cluster_abort(bs *BlockDriverState, m *QCowL2Meta) {
	//do nothing
}

func calculate_l2_meta(bs *BlockDriverState, hostClusterOffset uint64,
	guestOffset uint64, bytes uint32, l2Slice unsafe.Pointer, m **QCowL2Meta, keepOld bool) error {

	s := bs.opaque.(*BDRVQcow2State)
	l2Index := uint32(offset_to_l2_slice_index(s, guestOffset))
	var scIndex uint64
	var l2Entry, l2Bitmap uint64
	var cow_start_from, cow_start_to, cow_end_to, cow_end_from, nbClusters uint64
	cow_start_to = offset_into_cluster(s, guestOffset)
	cow_end_from = cow_start_to + uint64(bytes)
	nbClusters = size_to_clusters(s, cow_end_from)
	var old_m *QCowL2Meta = *m
	var scType QCow2SubclusterType
	var err error
	var cnt uint64

	var i uint32
	skip_cow := keepOld

	/* Check the type of all affected subclusters */
	for i = 0; i < uint32(nbClusters); i++ {
		l2Entry = get_l2_entry(s, l2Slice, l2Index+i)
		l2Bitmap = get_l2_bitmap(s, l2Slice, l2Index+i)
		if skip_cow {
			var write_from, write_to uint64
			write_from = max(cow_start_to, uint64(i<<s.ClusterBits))
			write_to = min(cow_end_from, uint64((i+1)<<s.ClusterBits))

			firstSc := offset_to_sc_index(s, write_from)
			lastSc := offset_to_sc_index(s, write_to-1)
			if cnt, err = qcow2_get_subcluster_range_type(bs, l2Entry, l2Bitmap,
				firstSc, &scType); err != nil {
				return err
			}
			/* Is any of the subclusters of type != QCOW2_SUBCLUSTER_NORMAL ? */
			if scType != QCOW2_SUBCLUSTER_NORMAL || firstSc+uint64(cnt) <= lastSc {
				skip_cow = false
			}
		} else {
			/* If we can't skip the cow we can still look for invalid entries */
			scType = qcow2_get_subcluster_type(bs, l2Entry, l2Bitmap, 0)
		}
		if scType == QCOW2_SUBCLUSTER_INVALID {
			return ERR_EIO
		}
	}

	if skip_cow {
		return nil
	}

	/* Get the L2 entry of the first cluster */
	l2Entry = get_l2_entry(s, l2Slice, l2Index)
	l2Bitmap = get_l2_bitmap(s, l2Slice, l2Index)
	scIndex = offset_to_sc_index(s, guestOffset)
	scType = qcow2_get_subcluster_type(bs, l2Entry, l2Bitmap, scIndex)

	if !keepOld {
		switch scType {
		case QCOW2_SUBCLUSTER_COMPRESSED:
			cow_start_from = 0
		case QCOW2_SUBCLUSTER_NORMAL, QCOW2_SUBCLUSTER_ZERO_ALLOC, QCOW2_SUBCLUSTER_UNALLOCATED_ALLOC:
			if has_subclusters(s) {
				/* Skip all leading zero and unallocated subclusters */
				allocBitmap := uint32(l2Bitmap & QCOW_L2_BITMAP_ALL_ALLOC)
				cow_start_from = min(scIndex, uint64(ctz32(allocBitmap))) << s.SubclusterBits
			} else {
				cow_start_from = 0
			}
		case QCOW2_SUBCLUSTER_ZERO_PLAIN, QCOW2_SUBCLUSTER_UNALLOCATED_PLAIN:
			cow_start_from = scIndex << s.SubclusterBits
		default:
			panic("unexpected")
		}
	} else {
		switch scType {
		case QCOW2_SUBCLUSTER_NORMAL:
			cow_start_from = cow_start_to
		case QCOW2_SUBCLUSTER_ZERO_ALLOC, QCOW2_SUBCLUSTER_UNALLOCATED_ALLOC:
			cow_start_from = scIndex << s.SubclusterBits
		default:
			panic("unexpected")
		}
	}

	/* Get the L2 entry of the last cluster */
	l2Index += uint32(nbClusters) - 1
	l2Entry = get_l2_entry(s, l2Slice, l2Index)
	l2Bitmap = get_l2_bitmap(s, l2Slice, l2Index)
	scIndex = offset_to_sc_index(s, guestOffset+uint64(bytes)-1)
	scType = qcow2_get_subcluster_type(bs, l2Entry, l2Bitmap, scIndex)

	if !keepOld {
		switch scType {
		case QCOW2_SUBCLUSTER_COMPRESSED:
			cow_end_to = round_up(cow_end_from, uint64(s.ClusterSize))

		case QCOW2_SUBCLUSTER_NORMAL, QCOW2_SUBCLUSTER_ZERO_ALLOC, QCOW2_SUBCLUSTER_UNALLOCATED_ALLOC:
			cow_end_to = round_up(cow_end_from, uint64(s.ClusterSize))
			if has_subclusters(s) {
				/* Skip all trailing zero and unallocated subclusters */
				allocBitmap := uint32(l2Bitmap & QCOW_L2_BITMAP_ALL_ALLOC)
				cow_end_to -= min(s.SubclustersPerCluster-scIndex-1,
					uint64(clz32(allocBitmap))) << s.SubclusterBits
			}

		case QCOW2_SUBCLUSTER_ZERO_PLAIN, QCOW2_SUBCLUSTER_UNALLOCATED_PLAIN:
			cow_end_to = round_up(cow_end_from, s.SubclusterSize)
		default:
			panic("unexpected")
		}
	} else {
		switch scType {
		case QCOW2_SUBCLUSTER_NORMAL:
			cow_end_to = cow_end_from
		case QCOW2_SUBCLUSTER_ZERO_ALLOC, QCOW2_SUBCLUSTER_UNALLOCATED_ALLOC:
			cow_end_to = round_up(cow_end_from, s.SubclusterSize)
		default:
			panic("unexpected")
		}
	}

	*m = &QCowL2Meta{
		Next:            old_m,
		Offset:          start_of_cluster(s, guestOffset),
		AllocOffset:     hostClusterOffset,
		NbClusters:      int(nbClusters),
		KeepOldClusters: keepOld,
		CowStart: Qcow2COWRegion{
			Offset:  cow_start_from,
			NbBytes: cow_start_to - cow_start_from,
		},
		CowEnd: Qcow2COWRegion{
			Offset:  cow_end_from,
			NbBytes: cow_end_to - cow_end_from,
		},
	}

	/*qemu_co_queue_init(&(*m)->dependent_requests);
	  QLIST_INSERT_HEAD(&s->cluster_allocs, *m, next_in_flight);
	*/
	//insert the *m to the head of the cluster allocs list
	//TODO1
	(*m).NextInFlight = s.ClusterAllocs.PushFront(*m)
	return nil
}

func cluster_needs_new_alloc(bs *BlockDriverState, l2Entry uint64) bool {

	switch qcow2_get_cluster_type(bs, l2Entry) {
	case QCOW2_CLUSTER_NORMAL, QCOW2_CLUSTER_ZERO_ALLOC:
		if l2Entry&QCOW_OFLAG_COPIED > 0 {
			return false
		}
	case QCOW2_CLUSTER_UNALLOCATED, QCOW2_CLUSTER_COMPRESSED, QCOW2_CLUSTER_ZERO_PLAIN:
		return true
	default:
		panic("Unexpected")
	}
	return true
}

func count_single_write_clusters(bs *BlockDriverState, nbClusters uint32,
	l2Slice unsafe.Pointer /*[]uint64*/, l2Index uint32, newAlloc bool) uint32 {

	s := bs.opaque.(*BDRVQcow2State)
	l2Entry := get_l2_entry(s, l2Slice, l2Index)
	expectedOffset := l2Entry & L2E_OFFSET_MASK
	var i uint32

	for i = 0; i < nbClusters; i++ {
		l2Entry = get_l2_entry(s, l2Slice, l2Index+i)
		if cluster_needs_new_alloc(bs, l2Entry) != newAlloc {
			break
		}
		if !newAlloc {
			if expectedOffset != (l2Entry & L2E_OFFSET_MASK) {
				break
			}
			expectedOffset += uint64(s.ClusterSize)
		}
	}
	return i
}

func do_alloc_cluster_offset(bs *BlockDriverState, guestOffset uint64, hostOffset *uint64, nbClusters *uint64) error {
	s := bs.opaque.(*BDRVQcow2State)
	var clusterOffset, n uint64
	var err error
	if *hostOffset == INV_OFFSET {
		if clusterOffset, err = qcow2_alloc_clusters(bs, *nbClusters*uint64(s.ClusterSize)); err != nil {
			return err
		}
		*hostOffset = uint64(clusterOffset)
	} else {
		if n, err = qcow2_alloc_clusters_at(bs, *hostOffset, int64(*nbClusters)); err != nil {
			return err
		}
		*nbClusters = uint64(n)
	}
	return nil
}

func qcow2_alloc_host_offset(bs *BlockDriverState, offset uint64,
	bytes *uint64, hostOffset *uint64, m **QCowL2Meta) error {

	//s := bs.opaque
	var start, remaining uint64
	var clusterOffset uint64
	var curBytes uint64
	var err error

	//again:
	start = offset
	remaining = *bytes
	clusterOffset = INV_OFFSET
	*hostOffset = INV_OFFSET
	curBytes = 0
	*m = nil
	for {
		if *hostOffset == INV_OFFSET && clusterOffset != INV_OFFSET {
			*hostOffset = clusterOffset
		}
		start += curBytes
		remaining -= curBytes

		if clusterOffset != INV_OFFSET {
			clusterOffset += curBytes
		}
		if remaining == 0 {
			break
		}
		curBytes = remaining

		//TODO2
		/*err = Handle_Dependencies(bs, start, &curBytes, m)
		err = nil
		if err == syscall.EAGAIN {
			goto again
		} else if err != nil {
			return err
		} else if curBytes == 0 {
			break
		} else {

		} */

		var ret uint64
		/*
		 * 2. Count contiguous COPIED clusters.
		 */
		ret, err = handle_copied(bs, start, &clusterOffset, &curBytes, m)
		if err != nil {
			return err
		} else if ret > 0 {
			continue
		} else if curBytes == 0 {
			break
		}

		/*
		 * 3. If the request still hasn't completed, allocate new clusters,
		 *    considering any cluster_offset of steps 1c or 2.
		 */
		ret, err := handle_alloc(bs, start, &clusterOffset, &curBytes, m)
		if err != nil {
			return err
		} else if ret > 0 {
			continue
		} else {
			break
		}
	} //end for

	*bytes -= remaining
	return err
}

func handle_alloc(bs *BlockDriverState, guestOffset uint64,
	hostOffset *uint64, bytes *uint64, m **QCowL2Meta) (uint64, error) {

	s := bs.opaque.(*BDRVQcow2State)
	var l2Index uint32
	var l2Slice unsafe.Pointer
	var nbClusters uint64
	var ret uint64
	var err error
	var allocClusterOffset uint64
	var requestedBytes, availBytes uint64
	var nbBytes uint64

	nbClusters = size_to_clusters(s, offset_into_cluster(s, guestOffset)+*bytes)
	l2Index = uint32(offset_to_l2_slice_index(s, guestOffset))
	nbClusters = min(nbClusters, uint64(s.L2SliceSize)-uint64(l2Index))

	/* Find L2 entry for the first involved cluster */
	if l2Slice, l2Index, err = get_cluster_table(bs, guestOffset); err != nil {
		return 0, err
	}

	nbClusters = uint64(count_single_write_clusters(bs, uint32(nbClusters), l2Slice, l2Index, true))

	/* Allocate at a given offset in the image file */
	if *hostOffset == INV_OFFSET {
		allocClusterOffset = INV_OFFSET
	} else {
		allocClusterOffset = start_of_cluster(s, *hostOffset)
	}

	if err = do_alloc_cluster_offset(bs, guestOffset, &allocClusterOffset, &nbClusters); err != nil {
		goto out
	}

	/* Can't extend contiguous allocation */
	if nbClusters == 0 {
		*bytes = 0
		ret = 0
		goto out
	}

	requestedBytes = *bytes + offset_into_cluster(s, guestOffset)
	availBytes = nbClusters << s.ClusterBits
	nbBytes = min(requestedBytes, availBytes)

	*hostOffset = allocClusterOffset + offset_into_cluster(s, guestOffset)
	*bytes = min(*bytes, nbBytes-offset_into_cluster(s, guestOffset))

	if err = calculate_l2_meta(bs, allocClusterOffset, guestOffset, uint32(*bytes),
		l2Slice, m, false); err != nil {
		goto out
	}
	ret = 1
	err = nil
out:
	qcow2_cache_put(s.L2TableCache, l2Slice)
	return ret, err
}

func handle_copied(bs *BlockDriverState, guestOffset uint64,
	hostOffset *uint64, bytes *uint64, m **QCowL2Meta) (uint64, error) {

	s := bs.opaque.(*BDRVQcow2State)
	var l2Index uint32
	var l2Entry, clusterOffset uint64
	var l2Slice unsafe.Pointer
	var nbClusters uint64
	var keepClusters uint32
	var ret uint64
	var err error

	nbClusters = size_to_clusters(s, offset_into_cluster(s, guestOffset)+*bytes)
	l2Index = uint32(offset_to_l2_slice_index(s, guestOffset))
	nbClusters = min(nbClusters, uint64(s.L2SliceSize)-uint64(l2Index))

	/* Find L2 entry for the first involved cluster */
	if l2Slice, l2Index, err = get_cluster_table(bs, guestOffset); err != nil {
		return 0, err
	}

	l2Entry = get_l2_entry(s, l2Slice, l2Index)
	clusterOffset = l2Entry & L2E_OFFSET_MASK

	if !cluster_needs_new_alloc(bs, l2Entry) {
		if offset_into_cluster(s, clusterOffset) > 0 {
			err = ERR_EIO
			goto out
		}
		/* If a specific host_offset is required, check it */
		if *hostOffset != INV_OFFSET && clusterOffset != *hostOffset {
			*bytes = 0
			ret = 0
			goto out
		}

		/* We keep all QCOW_OFLAG_COPIED clusters */
		keepClusters = count_single_write_clusters(bs, uint32(nbClusters), l2Slice, l2Index, false)
		if *bytes > uint64(keepClusters*s.ClusterSize)-offset_into_cluster(s, guestOffset) {
			*bytes = uint64(keepClusters*s.ClusterSize) - offset_into_cluster(s, guestOffset)
		}
		if err = calculate_l2_meta(bs, clusterOffset, guestOffset, uint32(*bytes), l2Slice, m, true); err != nil {
			goto out
		}
		ret = 1
	} else {
		ret = 0
	}
out:

	qcow2_cache_put(s.L2TableCache, l2Slice)

	/* Only return a host offset if we actually made progress. Otherwise we
	 * would make requirements for handle_alloc() that it can't fulfill */
	if ret > 0 {
		*hostOffset = clusterOffset + offset_into_cluster(s, guestOffset)
	}

	return ret, err
}

func qcow2_get_subcluster_range_type(bs *BlockDriverState, l2Entry uint64, l2Bitmap uint64,
	scFrom uint64, scType *QCow2SubclusterType) (uint64, error) {

	s := bs.opaque.(*BDRVQcow2State)
	var val uint32

	*scType = qcow2_get_subcluster_type(bs, l2Entry, l2Bitmap, scFrom)

	if *scType == QCOW2_SUBCLUSTER_INVALID {
		return 0, ERR_EINVAL
	} else if !has_subclusters(s) || *scType == QCOW2_SUBCLUSTER_COMPRESSED {
		return s.SubclustersPerCluster - scFrom, nil
	}

	switch *scType {
	case QCOW2_SUBCLUSTER_NORMAL:
		val = uint32(l2Bitmap) | uint32(qcow_oflag_sub_alloc_range(0, uint32(scFrom)))
		return uint64(cto32(val)) - scFrom, nil

	case QCOW2_SUBCLUSTER_ZERO_PLAIN, QCOW2_SUBCLUSTER_ZERO_ALLOC:
		val = uint32(l2Bitmap) | uint32(qcow_oflag_sub_zero_range(0, uint32(scFrom>>32)))
		return uint64(cto32(val)) - scFrom, nil

	case QCOW2_SUBCLUSTER_UNALLOCATED_PLAIN, QCOW2_SUBCLUSTER_UNALLOCATED_ALLOC:
		val = uint32(l2Bitmap>>32) | uint32(l2Bitmap)&uint32(^qcow_oflag_sub_alloc_range(0, uint32(scFrom)))
		return uint64(ctz32(val)) - scFrom, nil

	default:
		panic("unexpected")
	}
}

func qcow2_get_subcluster_type(bs *BlockDriverState, l2Entry uint64,
	l2Bitmap uint64, scIndex uint64) QCow2SubclusterType {

	s := bs.opaque.(*BDRVQcow2State)
	cType := qcow2_get_cluster_type(bs, l2Entry)

	//fmt.Printf("[qcow2_get_subcluster_type] l2Bitmap=%d, scIndex=%d, cType=%d\n", l2Bitmap, scIndex, cType)
	if has_subclusters(s) {
		switch cType {
		case QCOW2_CLUSTER_COMPRESSED:
			return QCOW2_SUBCLUSTER_COMPRESSED
		case QCOW2_CLUSTER_NORMAL:
			if ((l2Bitmap >> 32) & l2Bitmap) > 0 {
				return QCOW2_SUBCLUSTER_INVALID
			} else if int(l2Bitmap)&qcow_oflag_sub_zero(uint32(scIndex)) > 0 {
				return QCOW2_SUBCLUSTER_ZERO_ALLOC
			} else if int(l2Bitmap)&qcow_oflag_sub_alloc(uint32(scIndex)) > 0 {
				return QCOW2_SUBCLUSTER_NORMAL
			} else {
				return QCOW2_SUBCLUSTER_UNALLOCATED_ALLOC
			}
		case QCOW2_CLUSTER_UNALLOCATED:
			if l2Bitmap&QCOW_L2_BITMAP_ALL_ALLOC > 0 {
				return QCOW2_SUBCLUSTER_INVALID
			} else if int(l2Bitmap)&qcow_oflag_sub_zero(uint32(scIndex)) > 0 {
				return QCOW2_SUBCLUSTER_ZERO_PLAIN
			} else {
				return QCOW2_SUBCLUSTER_UNALLOCATED_PLAIN
			}
		default:
			panic("should not happen")
		}
	} else {
		switch cType {
		case QCOW2_CLUSTER_COMPRESSED:
			return QCOW2_SUBCLUSTER_COMPRESSED
		case QCOW2_CLUSTER_ZERO_PLAIN:
			return QCOW2_SUBCLUSTER_ZERO_PLAIN
		case QCOW2_CLUSTER_ZERO_ALLOC:
			return QCOW2_SUBCLUSTER_ZERO_ALLOC
		case QCOW2_CLUSTER_NORMAL:
			return QCOW2_SUBCLUSTER_NORMAL
		case QCOW2_CLUSTER_UNALLOCATED:
			return QCOW2_SUBCLUSTER_UNALLOCATED_PLAIN
		default:
			panic("should not happen")
		}
	}
}

func qcow2_get_cluster_type(bs *BlockDriverState, l2Entry uint64) QCow2ClusterType {
	s := bs.opaque.(*BDRVQcow2State)

	if l2Entry&QCOW_OFLAG_COMPRESSED > 0 {
		return QCOW2_CLUSTER_COMPRESSED
	} else if (l2Entry&QCOW_OFLAG_ZERO) > 0 && !has_subclusters(s) {
		if l2Entry&L2E_OFFSET_MASK > 0 {
			return QCOW2_CLUSTER_ZERO_ALLOC
		}
		return QCOW2_CLUSTER_ZERO_PLAIN
	} else if l2Entry&L2E_OFFSET_MASK == 0 {
		return QCOW2_CLUSTER_UNALLOCATED
	} else {
		return QCOW2_CLUSTER_NORMAL
	}
}

func qcow2_free_any_cluster(bs *BlockDriverState, l2Entry uint64) {

	s := bs.opaque.(*BDRVQcow2State)
	ctype := qcow2_get_cluster_type(bs, l2Entry)

	switch ctype {
	case QCOW2_CLUSTER_COMPRESSED:
		//do nothing
	case QCOW2_CLUSTER_NORMAL, QCOW2_CLUSTER_ZERO_ALLOC:
		if offset_into_cluster(s, l2Entry&L2E_OFFSET_MASK) > 0 {
			panic("Cannot free unaligned cluster")
		} else {
			qcow2_free_clusters(bs, l2Entry&L2E_OFFSET_MASK, uint64(s.ClusterSize))
		}
	case QCOW2_CLUSTER_ZERO_PLAIN, QCOW2_CLUSTER_UNALLOCATED:
		//do nothing
	default:
		panic("unexpected")
	}
}

func do_perform_cow_read(bs *BlockDriverState, srcClusterOffset uint64,
	offsetInCluster uint64, qiov *QEMUIOVector) error {

	var err error

	if qiov.size == 0 {
		return nil
	}
	if bs.Drv == nil || bs.Drv.bdrv_preadv_part == nil {
		return Err_NoDriverFound
	}
	if err = bs.Drv.bdrv_preadv_part(bs, srcClusterOffset+offsetInCluster,
		qiov.size, qiov, 0, 0); err != nil {
		return err
	}

	return nil
}

func do_perform_cow_write(bs *BlockDriverState, clusterOffset uint64,
	offsetInCluster uint64, qiov *QEMUIOVector) error {

	s := bs.opaque.(*BDRVQcow2State)
	var err error

	if qiov.size == 0 {
		return nil
	}
	if err = bdrv_pwritev(s.DataFile, clusterOffset+offsetInCluster,
		qiov.size, qiov, 0); err != nil {
		return err
	}
	return nil
}

func perform_cow(bs *BlockDriverState, m *QCowL2Meta) error {

	s := bs.opaque.(*BDRVQcow2State)
	var start *Qcow2COWRegion = &m.CowStart
	var end *Qcow2COWRegion = &m.CowEnd
	var bufferSize uint64
	var dataBytes uint64 = end.Offset - (start.Offset + start.NbBytes)
	var mergeReads bool
	var startBuffer, endBuffer []uint8
	var qiov QEMUIOVector
	var err error
	if (start.NbBytes == 0 && end.NbBytes == 0) || m.SkipCow {
		return nil
	}

	mergeReads = (start.NbBytes > 0 && end.NbBytes > 0 && dataBytes <= 16384)
	if mergeReads {
		bufferSize = start.NbBytes + dataBytes + end.NbBytes
	} else {
		align := bdrv_opt_mem_align(bs)
		bufferSize = align_up(start.NbBytes, align) + end.NbBytes
	}

	startBuffer = make([]uint8, bufferSize)
	/* The part of the buffer where the end region is located */
	endBuffer = startBuffer[bufferSize-end.NbBytes:]

	if m.DataQiov != nil {
		Qemu_Iovec_Init(&qiov, 2+Qemu_Iovec_Subvec_Niov(m.DataQiov, m.DataQiovOffset, dataBytes))
	} else {
		Qemu_Iovec_Init(&qiov, 2)
	}

	s.Lock.Unlock()

	if mergeReads {
		Qemu_Iovec_Add(&qiov, unsafe.Pointer(&startBuffer[0]), bufferSize)
		err = do_perform_cow_read(bs, m.Offset, start.Offset, &qiov)
	} else {
		Qemu_Iovec_Add(&qiov, unsafe.Pointer(&startBuffer[0]), start.NbBytes)
		if err = do_perform_cow_read(bs, m.Offset, start.Offset, &qiov); err != nil {
			goto fail
		}

		Qemu_Iovec_Reset(&qiov)
		Qemu_Iovec_Add(&qiov, unsafe.Pointer(&endBuffer[0]), end.NbBytes)
		err = do_perform_cow_read(bs, m.Offset, end.Offset, &qiov)
	}
	if err != nil {
		goto fail
	}

	if m.DataQiov != nil {
		Qemu_Iovec_Reset(&qiov)
		if start.NbBytes > 0 {
			Qemu_Iovec_Add(&qiov, unsafe.Pointer(&startBuffer[0]), start.NbBytes)
		}
		Qemu_Iovec_Concat(&qiov, m.DataQiov, m.DataQiovOffset, dataBytes)
		if end.NbBytes > 0 {
			Qemu_Iovec_Add(&qiov, unsafe.Pointer(&endBuffer[0]), end.NbBytes)
		}
		err = do_perform_cow_write(bs, m.AllocOffset, start.Offset, &qiov)
	} else {
		/* If there's no guest data then write both COW regions separately */
		Qemu_Iovec_Reset(&qiov)
		Qemu_Iovec_Add(&qiov, unsafe.Pointer(&startBuffer[0]), start.NbBytes)
		if err = do_perform_cow_write(bs, m.AllocOffset, start.Offset, &qiov); err != nil {
			goto fail
		}

		Qemu_Iovec_Reset(&qiov)
		Qemu_Iovec_Add(&qiov, unsafe.Pointer(&endBuffer[0]), end.NbBytes)
		err = do_perform_cow_write(bs, m.AllocOffset, end.Offset, &qiov)
	}
fail:
	s.Lock.Lock()
	if err == nil {
		qcow2_cache_depends_on_flush(s.L2TableCache)
	}

	Qemu_Iovec_Destroy(&qiov)
	return err
}

func count_contiguous_subclusters(bs *BlockDriverState, nbClusters uint64, scIndex uint64,
	l2Slice unsafe.Pointer, l2Index *uint64) (uint64, error) {

	s := bs.opaque.(*BDRVQcow2State)
	var i, count uint64
	checkOffset := false
	var expectedOffset uint64
	var expectedType QCow2SubclusterType = QCOW2_SUBCLUSTER_NORMAL
	var tmpType QCow2SubclusterType
	var ret uint64
	var err error

	for i = 0; i < nbClusters; i++ {
		firstSc := scIndex
		if i > 0 {
			firstSc = 0
		}

		l2Entry := get_l2_entry(s, l2Slice, uint32(*l2Index+i))
		l2Bitmap := get_l2_bitmap(s, l2Slice, uint32(*l2Index+i))

		if ret, err = qcow2_get_subcluster_range_type(bs, l2Entry, l2Bitmap, firstSc, &tmpType); err != nil {
			*l2Index += i /* Point to the invalid entry */
			return 0, ERR_EIO
		}
		if i == 0 {
			if tmpType == QCOW2_SUBCLUSTER_COMPRESSED {
				return uint64(ret), nil
			}
			expectedType = tmpType
			expectedOffset = l2Entry & L2E_OFFSET_MASK
			checkOffset = (tmpType == QCOW2_SUBCLUSTER_NORMAL ||
				tmpType == QCOW2_SUBCLUSTER_ZERO_ALLOC ||
				tmpType == QCOW2_SUBCLUSTER_UNALLOCATED_ALLOC)
		} else if tmpType != expectedType {
			break
		} else if checkOffset {
			expectedOffset += uint64(s.ClusterSize)
			if expectedOffset != (l2Entry & L2E_OFFSET_MASK) {
				break
			}
		}

		count += ret
		/* Stop if there are type changes before the end of the cluster */
		if firstSc+ret < s.SubclustersPerCluster {
			break
		}
	} //end for

	return count, nil
}

func qcow2_subcluster_zeroize(bs *BlockDriverState, offset uint64, bytes uint64, flags int) error {

	s := bs.opaque.(*BDRVQcow2State)
	var endOffset uint64 = offset + bytes
	var nbClusters uint64
	var head, tail uint64
	var cleared uint64
	var err error
	head = min(endOffset, round_up(offset, uint64(s.ClusterSize))) - offset
	offset += head

	if endOffset >= bs.TotalSectors<<BDRV_SECTOR_BITS {
		tail = 0
	} else {
		tail = endOffset - max(offset, start_of_cluster(s, endOffset))
	}
	endOffset -= tail

	if head > 0 {
		if err = zero_l2_subclusters(bs, offset-head, size_to_subclusters(s, head)); err != nil {
			goto fail
		}
	}

	/* Each L2 slice is handled by its own loop iteration */
	nbClusters = size_to_clusters(s, endOffset-offset)

	for nbClusters > 0 {
		if cleared, err = zero_in_l2_slice(bs, offset, nbClusters, flags); err != nil {
			goto fail
		}
		nbClusters -= cleared
		offset += cleared * uint64(s.ClusterSize)
	}

	if tail > 0 {
		if err = zero_l2_subclusters(bs, endOffset, size_to_subclusters(s, tail)); err != nil {
			goto fail
		}
	}

	err = nil
fail:
	return err
}

func zero_in_l2_slice(bs *BlockDriverState, offset uint64, nbClusters uint64, flags int) (uint64, error) {

	s := bs.opaque.(*BDRVQcow2State)
	var l2Slice unsafe.Pointer
	var l2Index uint32
	var err error
	var i uint32

	if l2Slice, l2Index, err = get_cluster_table(bs, offset); err != nil {
		return 0, err
	}

	/* Limit nb_clusters to one L2 slice */
	nbClusters = min(nbClusters, uint64(s.L2SliceSize)-uint64(l2Index))

	for i = 0; i < uint32(nbClusters); i++ {
		oldL2Entry := get_l2_entry(s, l2Slice, l2Index+i)
		oldL2Bitmap := get_l2_bitmap(s, l2Slice, l2Index+i)
		var ctype QCow2ClusterType = qcow2_get_cluster_type(bs, oldL2Entry)
		var unmap bool = (ctype == QCOW2_CLUSTER_COMPRESSED) ||
			((flags&BDRV_REQ_MAY_UNMAP) > 0 && qcow2_cluster_is_allocated(ctype))

		var newL2Entry, newL2Bitmap uint64
		if unmap {
			newL2Entry = 0
		} else {
			newL2Entry = oldL2Entry
		}
		newL2Bitmap = oldL2Bitmap

		if has_subclusters(s) {
			newL2Bitmap = QCOW_L2_BITMAP_ALL_ZEROES
		} else {
			newL2Entry |= QCOW_OFLAG_ZERO
		}

		if oldL2Entry == newL2Entry && oldL2Bitmap == newL2Bitmap {
			continue
		}

		/* First update L2 entries */
		qcow2_cache_entry_mark_dirty(s.L2TableCache, l2Slice)
		set_l2_entry(s, l2Slice, l2Index+i, newL2Entry)
		if has_subclusters(s) {
			set_l2_bitmap(s, l2Slice, l2Index+i, newL2Bitmap)
		}

		/* Then decrease the refcount */
		if unmap {
			qcow2_free_any_cluster(bs, oldL2Entry)
		}
	}

	qcow2_cache_put(s.L2TableCache, l2Slice)

	return nbClusters, err

}

func zero_l2_subclusters(bs *BlockDriverState, offset uint64, nbSubclusters uint64) error {

	s := bs.opaque.(*BDRVQcow2State)
	// uint64_t *l2_slice;
	var l2Slice unsafe.Pointer
	var oldL2Bitmap, l2Bitmap uint64
	var l2Index uint32
	var err error
	sc := offset_to_sc_index(s, offset)

	if l2Slice, l2Index, err = get_cluster_table(bs, offset); err != nil {
		return err
	}

	switch qcow2_get_cluster_type(bs, get_l2_entry(s, l2Slice, l2Index)) {
	case QCOW2_CLUSTER_COMPRESSED:
		err = ERR_ENOTSUP
		goto out
	case QCOW2_CLUSTER_NORMAL, QCOW2_CLUSTER_UNALLOCATED:
		break
	default:
		panic("unexpected")
	}

	l2Bitmap = get_l2_bitmap(s, l2Slice, l2Index)
	oldL2Bitmap = l2Bitmap
	l2Bitmap |= uint64(qcow_oflag_sub_zero_range(uint32(sc), uint32(sc+nbSubclusters)))
	l2Bitmap &= uint64(^qcow_oflag_sub_alloc_range(uint32(sc), uint32(sc+nbSubclusters)))

	if oldL2Bitmap != l2Bitmap {
		set_l2_bitmap(s, l2Slice, l2Index, l2Bitmap)
		qcow2_cache_entry_mark_dirty(s.L2TableCache, l2Slice)
	}
	err = nil
out:
	qcow2_cache_put(s.L2TableCache, l2Slice)
	return err
}

func qcow2_cluster_is_allocated(ctype QCow2ClusterType) bool {
	return (ctype == QCOW2_CLUSTER_COMPRESSED || ctype == QCOW2_CLUSTER_NORMAL ||
		ctype == QCOW2_CLUSTER_ZERO_ALLOC)
}
