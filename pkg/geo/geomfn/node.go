// Copyright 2020 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package geomfn

import (
	"github.com/cockroachdb/cockroach/pkg/geo"
	"github.com/cockroachdb/cockroach/pkg/geo/geopb"
	"github.com/cockroachdb/cockroach/pkg/geo/geos"
	"github.com/cockroachdb/errors"
	"github.com/twpayne/go-geom"
	"github.com/twpayne/go-geom/xy"
)

// Node returns a geometry containing a set of LineStrings using the least
// possible number of nodes while preserving all of the input ones.
func Node(g geo.Geometry) (geo.Geometry, error) {
	if g.ShapeType() != geopb.ShapeType_LineString && g.ShapeType() != geopb.ShapeType_MultiLineString {
		return geo.Geometry{}, errors.New("geometry type is unsupported. Please pass a LineString or a MultiLineString")
	}

	res, err := geos.Node(g.EWKB())
	if err != nil {
		return geo.Geometry{}, err
	}
	node, err := geo.ParseGeometryFromEWKB(res)
	if err != nil {
		return geo.Geometry{}, err
	}

	res, err = geos.LineMerge(node.EWKB())
	if err != nil {
		return geo.Geometry{}, err
	}
	lines, err := geo.ParseGeometryFromEWKB(res)
	if err != nil {
		return geo.Geometry{}, err
	}
	if lines.ShapeType() == geopb.ShapeType_LineString {
		// No nodes found, return a MultiLineString.
		return node, nil
	}

	glines, err := lines.AsGeomT()
	if err != nil {
		return geo.Geometry{}, errors.Newf("error transforming lines: %v", err)
	}
	ep, err := extractEndpoints(g)
	if err != nil {
		return geo.Geometry{}, errors.Newf("error extracting endpoints: %v", err)
	}
	mllines := glines.(*geom.MultiLineString)

	gep, err := ep.AsGeomT()
	if err != nil {
		return geo.Geometry{}, errors.Newf("error transforming endpoints: %v", err)
	}
	mpep := gep.(*geom.MultiPoint)
	mlout, err := splitLinesByPoints(mllines, mpep)
	if err != nil {
		return geo.Geometry{}, err
	}
	mlout.SetSRID(int(g.SRID()))
	out, err := geo.MakeGeometryFromGeomT(mlout)
	if err != nil {
		return geo.Geometry{}, errors.Newf("could not transform output into geometry: %v", err)
	}
	return out, nil
}

// splitLinesByPoints goes through every LineString and tries to split that LineString by any
// of the Points provided. Does not split LineString if the Point is an endpoint for that line.
// Returns MultiLineString consisting of splitted, as well as not splitted provided LineStrings.
func splitLinesByPoints(
	mllines *geom.MultiLineString, mpep *geom.MultiPoint,
) (*geom.MultiLineString, error) {
	mlout := geom.NewMultiLineString(geom.XY)
	splitted := false
	var err error
	var splitLines []*geom.LineString
	for i := 0; i < mllines.NumLineStrings(); i++ {
		l := mllines.LineString(i)
		for j := 0; j < mpep.NumPoints(); j++ {
			p := mpep.Point(j)
			splitted, splitLines, err = splitLineByPoint(l, p.Coords())
			if err != nil {
				return nil, errors.Newf("could not split line: %v", err)
			}
			if splitted {
				err = mlout.Push(splitLines[0])
				if err != nil {
					return nil, errors.Newf("could not construct output geometry: %v", err)
				}
				err = mlout.Push(splitLines[1])
				if err != nil {
					return nil, errors.Newf("could not construct output geometry: %v", err)
				}
				break
			}
		}
		if !splitted {
			err = mlout.Push(l)
			if err != nil {
				return nil, errors.Newf("could not construct output geometry: %v", err)
			}
		}
	}
	return mlout, nil
}

// extractEndpoints extracts the endpoints from geometry provided and returns them as a MultiPoint geometry.
func extractEndpoints(g geo.Geometry) (geo.Geometry, error) {
	mp := geom.NewMultiPoint(geom.XY)

	gt, err := g.AsGeomT()
	if err != nil {
		return geo.Geometry{}, errors.Newf("error transforming geometry: %v", err)
	}

	switch gt := gt.(type) {
	case *geom.LineString:
		endpoints := collectEndpoints(gt)
		for _, endpoint := range endpoints {
			err := mp.Push(endpoint)
			if err != nil {
				return geo.Geometry{}, errors.Newf("error creating output geometry: %v", err)
			}
		}
	case *geom.MultiLineString:
		for i := 0; i < gt.NumLineStrings(); i++ {
			ls := gt.LineString(i)
			endpoints := collectEndpoints(ls)
			for _, endpoint := range endpoints {
				err := mp.Push(endpoint)
				if err != nil {
					return geo.Geometry{}, errors.Newf("error creating output geometry: %v", err)
				}
			}
		}
	default:
		return geo.Geometry{}, errors.Newf("unsupported type: %T", gt)
	}

	result, err := geo.MakeGeometryFromGeomT(mp)
	if err != nil {
		return geo.Geometry{}, errors.Newf("error creating output geometry: %v", err)
	}
	return result, nil
}

// collectEndpoints returns endpoints of the line provided as a slice of Points.
func collectEndpoints(ls *geom.LineString) []*geom.Point {
	coord := ls.Coord(0)
	startPoint := geom.NewPointFlat(geom.XY, []float64{coord.X(), coord.Y()})
	coord = ls.Coord(ls.NumCoords() - 1)
	endPoint := geom.NewPointFlat(geom.XY, []float64{coord.X(), coord.Y()})
	return []*geom.Point{startPoint, endPoint}
}

// splitLineByPoint splits the line using the Point provided.
// Returns a bool representing whether the line was splitted or not, and a slice of output LineStrings.
// The Point must be on provided line and not an endpoint, otherwise false is returned along with unsplitted line.
func splitLineByPoint(l *geom.LineString, p geom.Coord) (bool, []*geom.LineString, error) {
	// Do not split if Point is not on line.
	if !xy.IsOnLine(l.Layout(), p, l.FlatCoords()) {
		return false, []*geom.LineString{l}, nil
	}
	// Do not split if Point is the endpoint of the line.
	startCoord := l.Coord(0)
	endCoord := l.Coord(l.NumCoords() - 1)
	if p.Equal(l.Layout(), startCoord) || p.Equal(l.Layout(), endCoord) {
		return false, []*geom.LineString{l}, nil
	}
	// Find where to split the line and group coords.
	coordsA := []geom.Coord{}
	coordsB := []geom.Coord{}
	for i := 1; i < l.NumCoords(); i++ {
		if xy.IsPointWithinLineBounds(p, l.Coord(i-1), l.Coord(i)) {
			coordsA = append(l.Coords()[0:i], p)
			if p.Equal(l.Layout(), l.Coord(i)) {
				coordsB = l.Coords()[i:]
			} else {
				coordsB = append([]geom.Coord{p}, l.Coords()[i:]...)
			}
			break
		}
	}
	l1 := geom.NewLineString(l.Layout())
	_, err := l1.SetCoords(coordsA)
	if err != nil {
		return false, []*geom.LineString{}, errors.Newf("could not set coords: %v", err)
	}
	l2 := geom.NewLineString(l.Layout())
	_, err = l2.SetCoords(coordsB)
	if err != nil {
		return false, []*geom.LineString{}, errors.Newf("could not set coords: %v", err)
	}
	return true, []*geom.LineString{l1, l2}, nil
}
