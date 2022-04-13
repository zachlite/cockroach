import React, { createRef } from "react";
import { connect } from "react-redux";
import { AdminUIState } from "oss/src/redux/state";
import { HistoricalHotRangeResponseMessage } from "oss/src/util/api";
import _, { isEqual, sample, sampleSize } from "lodash";

// a few things to implement:
// 1) a connected container
// 2) the hot ranges canvas

// things will changes, so this should be componentized as much as possible.

// important things for me to do:
// implement protobuf so I can start plumbing data
// protobuf implements window to request data
// setting the window will be the responsibility of the client
// all of this is a layer on top of the visualization.

// in the future, we might have metrics per key.
// this would then become a "key visualizer"
interface RangeVisualizerProps {
  samples: {
    timestamp: number;

    // keys are sorted in ascending order.
    keys: string[];
    values: number[];
    // ranges: { startKey: string; qps: number }[];
  }[];
}

// for now, hardcode canvas width and height
// TODO: these values will not accomodate 2 weeks worth of samples (x)
// and 1000 ranges (y). Cell widths and heights respectively will be less than 1px.
const CanvasWidth = 1344;
const CanvasHeight = 729;


const ColorCold = 0;

class RangeVisualizer extends React.Component<RangeVisualizerProps> {
  // a canvas is set up here
  private canvasRef: React.RefObject<HTMLCanvasElement>;
  private drawContext: CanvasRenderingContext2D;

  constructor(props: RangeVisualizerProps) {
    super(props);
    this.canvasRef = React.createRef<HTMLCanvasElement>();
  }

  drawCell(
    canvasBufferData: Uint8ClampedArray,
    sampleIdx: number,
    keyIdx: number,
    color: number,
    bucketWidth: number
  ) {
    // TODO: how tall should a bucket be?
    // a bucket height should be 10 px?
    // no relationship to the size of the keyspace?
    const bucketHeight = 1;

    // We need to manipulate raw pixel values in the canvas's buffer
    // There are 4 bytes per pixel. red, green, blue, alpha.
    const startIndex = keyIdx * CanvasWidth * 4 + sampleIdx * bucketWidth * 4;
    const endIndex = startIndex + bucketWidth * 4 - 1;

    for (let pidx = startIndex; pidx <= endIndex; pidx += 4) {
      canvasBufferData[pidx] = color * 255; // red
      canvasBufferData[pidx + 1] = color * 255; // green
      canvasBufferData[pidx + 2] = color * 255; // blue
      canvasBufferData[pidx + 3] = 255; // alpha
    }
  }

  draw() {
    const start = window.performance.now();
    const keyspace = new Set<string>();
    const keysForSample = {} as Record<number, Set<string>>;
    let hottestValue = 0.0;

    for (let i = 0; i < this.props.samples.length; i++) {
      const sample = this.props.samples[i];
      for (const key of sample.keys) {
        keyspace.add(key);
      }

      // convert list of keys into a set for later O(1) lookups.
      keysForSample[i] = new Set(sample.keys);

      // find hottest value
      hottestValue = Math.max(hottestValue, ...sample.values);
    }

    console.log(keyspace);
    console.log("hottest value: ", hottestValue);

    const bucketWidth = Math.floor(CanvasWidth / this.props.samples.length);


    const canvasBuffer = this.drawContext.getImageData(
      0,
      0,
      CanvasWidth,
      CanvasHeight
    );

    const canvasBufferData = canvasBuffer.data;

    for (let i = 0; i < this.props.samples.length; i++) {
      let keyIdx = 0;
      let bucketIdx = 0;

      for (let key of keyspace) {
        if (keysForSample[i].has(key)) {
          const colorValue = this.props.samples[i].values[bucketIdx] / hottestValue
          this.drawCell(canvasBufferData, i, keyIdx, colorValue, bucketWidth);
          bucketIdx++;
        } else {
          this.drawCell(canvasBufferData, i, keyIdx, ColorCold, bucketWidth);
        }
        keyIdx++;
      }
    }

    this.drawContext.putImageData(canvasBuffer, 0, 0);
    const end = window.performance.now();
    alert(end-start)
  }

  componentDidMount() {
    this.drawContext = this.canvasRef.current.getContext("2d");

    this.drawContext.clearRect(0, 0, CanvasWidth, CanvasHeight);

    // draw background
    this.drawContext.fillStyle = "#000";
    this.drawContext.fillRect(0, 0, CanvasWidth, CanvasHeight);

    this.draw();
  }

  componentDidUpdate(prevProps: RangeVisualizerProps) {
    const previousTimestamps = prevProps.samples.map(
      (sample) => sample.timestamp
    );
    const currentTimestamps = this.props.samples.map(
      (sample) => sample.timestamp
    );

    if (!isEqual(previousTimestamps, currentTimestamps)) {
      this.draw();
    }
  }

  shouldComponentUpdate() {
    return false;
  }

  render() {
    console.warn("range visualizer render");

    return (
      <canvas
        width={CanvasWidth}
        height={CanvasHeight}
        ref={this.canvasRef}
      />
    );
  }
}

const alphabet = "abcdefghijklmnopqrstuvwxyz";

function randn_bm():number {
  let u = 0, v = 0;
  while(u === 0) u = Math.random(); //Converting [0,1) to (0,1)
  while(v === 0) v = Math.random();
  let num = Math.sqrt( -2.0 * Math.log( u ) ) * Math.cos( 2.0 * Math.PI * v );
  num = num / 10.0 + 0.5; // Translate to 0 -> 1
  if (num > 1 || num < 0) return randn_bm() // resample between 0 and 1
  return num
}


function getFakeKey() {
  let key = "";
  for (let i = 0; i < 3; i++) {
    const idx = Math.floor(Math.random() * 9); // limit keyspace to 10^3 unique values
    key += alphabet[idx];
  }
  return key;
}
class HistoricalHotRangesContainer extends React.Component<{
  hhrData: HistoricalHotRangeResponseMessage;
}> {
  componentDidMount() {
    // request HHR for initial time window
    // the initial state dictates the initial time window.
  }

  makeFakeHHRData() {
    // used to fake 4 samples (1 hour)
    // 672 - 1 week.
    // all the way to 1344 samples (2 weeks)
    const Hour = 4;
    const Day = Hour * 24;
    const Week = Day * 7
    const Full = Week * 2;
    
    const NSamples = Full;

    const samples = [];

    for (let i = 0; i < NSamples; i++) {
      // get 80 fake keys and values
      const keys = [];
      const values = [];

      const NRanges = randn_bm() * 1000;

      for (let k = 0; k < NRanges; k++) {
        keys.push(getFakeKey());
        values.push(randn_bm() * 100);
      }

      keys.sort();

      const sample = {
        timestamp: i,
        keys,
        values,
      };

      samples.push(sample);
    }

    return samples;
  }

  render() {
    console.log("hhr", this.props.hhrData);
    return <RangeVisualizer samples={this.makeFakeHHRData()} />;
  }
}

export const ConnectedHistoricalHotRangeContainer = connect(
  (state: AdminUIState) => {
    return {
      hhrData: state.cachedData.historicalHotRanges.data,
    };
  },
  {}
)(HistoricalHotRangesContainer);
