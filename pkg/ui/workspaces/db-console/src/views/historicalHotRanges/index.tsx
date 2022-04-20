import React from "react";
import { connect } from "react-redux";
import { Dispatch } from "redux";
import { AdminUIState } from "oss/src/redux/state";
import {
  getHistoricalHotRanges,
  HistoricalHotRangeResponseMessage,
} from "oss/src/util/api";
import _, { isEqual } from "lodash";
import { refreshHHR } from "oss/src/redux/apiReducers";
import { cockroach } from "src/js/protos";
const HHRRequest = cockroach.server.serverpb.HHRRequest;
const Timestamp = cockroach.util.hlc.Timestamp;

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
  samples: HistoricalHotRangeResponseMessage["samples"];
}

// for now, hardcode canvas width and height
// TODO: these values will not accomodate 2 weeks worth of samples (x)
// and 1000 ranges (y). Cell widths and heights respectively will be less than 1px.
const CanvasWidth = 1344;
const CanvasHeight = 4096;

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

  drawSample(
    i: number,
    buffer: Uint8ClampedArray,
    keyspace: any,
    keysForSample: any,
    bucketWidth: number,
    hottestValue: number
  ) {
    let keyIdx = 0;
    let bucketIdx = 0;

    for (let key of keyspace) {
      if (keysForSample[i].has(key)) {
        const colorValue = this.props.samples[i].qps[bucketIdx] / hottestValue;
        this.drawCell(buffer, i, keyIdx, colorValue, bucketWidth);
        bucketIdx++;
      } else {
        this.drawCell(buffer, i, keyIdx, ColorCold, bucketWidth);
      }
      keyIdx++;
    }
  }

  draw() {
    const start = window.performance.now();
    const keyspace = new Set<string>();
    const keysForSample = {} as Record<number, Set<string>>;
    let hottestValue = 0.0;

    for (let i = 0; i < this.props.samples.length; i++) {
      const sample = this.props.samples[i];
      for (const key of sample.start_key) {
        keyspace.add(key);
      }

      // convert list of keys into a set for later O(1) lookups.
      keysForSample[i] = new Set(sample.start_key);

      // find hottest value
      hottestValue = Math.max(hottestValue, ...sample.qps);
    }

    console.log(keyspace);
    console.log("hottest value: ", hottestValue);

    const bucketWidth = Math.floor(CanvasWidth / this.props.samples.length);
    console.log("bucket width: ", bucketWidth);

    const canvasBuffer = this.drawContext.getImageData(
      0,
      0,
      CanvasWidth,
      CanvasHeight
    );

    const canvasBufferData = canvasBuffer.data;

    for (let i = 0; i < this.props.samples.length; i++) {
      this.drawSample(
        i,
        canvasBufferData,
        keyspace,
        keysForSample,
        bucketWidth,
        hottestValue
      );
    }

    this.drawContext.putImageData(canvasBuffer, 0, 0);
    const end = window.performance.now();
    console.log("Draw time: ", end - start);
  }

  componentDidMount() {
    this.drawContext = this.canvasRef.current.getContext("2d");

    this.drawContext.clearRect(0, 0, CanvasWidth, CanvasHeight);

    // draw background
    this.drawContext.fillStyle = "#000";
    this.drawContext.fillRect(0, 0, CanvasWidth, CanvasHeight);

    this.draw();
  }

  componentDidUpdate() {
    this.drawContext.clearRect(0, 0, CanvasWidth, CanvasHeight);
    this.draw();
  }

  // shouldComponentUpdate(nextProps: RangeVisualizerProps) {
  //   const currentTimestamps = this.props.samples.map(
  //     (sample) => sample.timestamp
  //   );

  //   const nextTimestamps = nextProps.samples.map((sample) => sample.timestamp);

  //   if (!isEqual(nextTimestamps, currentTimestamps)) {
  //     this.draw();
  //   }

  //   return false;
  // }

  render() {
    console.warn("range visualizer render");

    return (
      <canvas width={CanvasWidth} height={CanvasHeight} ref={this.canvasRef} />
    );
  }
}

const alphabet = "abcdefghijklmnopqrstuvwxyz";

function randn_bm(): number {
  let u = 0,
    v = 0;
  while (u === 0) u = Math.random(); //Converting [0,1) to (0,1)
  while (v === 0) v = Math.random();
  let num = Math.sqrt(-2.0 * Math.log(u)) * Math.cos(2.0 * Math.PI * v);
  num = num / 10.0 + 0.5; // Translate to 0 -> 1
  if (num > 1 || num < 0) return randn_bm(); // resample between 0 and 1
  return num;
}

function getFakeKey() {
  let key = "";
  for (let i = 0; i < 3; i++) {
    const idx = Math.floor(Math.random() * 9); // limit keyspace to 10^3 unique values
    key += alphabet[idx];
  }
  return key;
}

interface HistoricalHotRangesContainerProps {
  hhrData: HistoricalHotRangeResponseMessage;
  fetchHHR: () => void;
}

interface HistoricalHotRangesContainerState {
  hhrData: HistoricalHotRangeResponseMessage["samples"];
}

class HistoricalHotRangesContainer extends React.Component<
  HistoricalHotRangesContainerProps,
  HistoricalHotRangesContainerState
> {
  constructor(props: HistoricalHotRangesContainerProps) {
    super(props);
    this.state = { hhrData: [] };
  }

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
    const Week = Day * 7;
    const Full = Week * 2;

    const NSamples = Hour;

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
    console.log("HHR Container ReRender");

    return (
      <>
        <button
          onClick={async () => {
            // test 1:
            // do this 56 times synchronously, where each samples represents 6 hours
            // (24 samples per response)

            // test 2:
            // do this 2 times synchronously, where each sample represents 168 hours (1 week)
            // 672 samples per response -> will need to update fake response

            let hhr = [] as any[];
            const start = window.performance.now();
            for (let i = 0; i < 1; i++) {
              const res = await getHistoricalHotRanges(
                HHRRequest.create({
                  tMin: Timestamp.create(),
                  tMax: Timestamp.create(),
                })
              );

              console.log("done with request ", i);
              for (const sample of res.samples) {
                hhr.push(sample);
              }
            }

            const end = window.performance.now();
            console.log("network time: ", end - start);

            // update state to trigger a re-render.
            this.setState({ hhrData: hhr });
          }}
        >
          Fetch stuff
        </button>
        <RangeVisualizer samples={this.state.hhrData} />
      </>
    );
  }
}

// historical data is requested in batches
// a request is constructed to ask the server for samples within a timeframe
// because of this, it is the client's responsibility to request a responsible amount of data at once.
// long term, is this a bad idea? should the API provide bandwidth safeguard?

// AdminUIState will hold HHR data keyed by timestamp.
// at most 1344 keys for full 2 week sample.
// this isn't too big to filter.

// A use case I want to optimize for:
// If I expand the time window from 1 hour to 6 hours, I should only need to download 5 hours worth of new data.

// a cached data reducer is only going to invalidate the cache after a certain time period
// So, I need to deal with data expiry later.
// the thing I need to do right now is send 56 requests for data, with each window being 6 hours.

export const ConnectedHistoricalHotRangeContainer = connect(
  (state: AdminUIState) => {
    return {
      hhrData: state.cachedData.historicalHotRanges.data,
    };
  },
  (dispatch: Dispatch) => {
    return {
      fetchHHR: () =>
        dispatch(
          refreshHHR(
            HHRRequest.create({
              tMin: Timestamp.create(),
              tMax: Timestamp.create(),
            })
          ) as any
        ),
    };
  }
)(HistoricalHotRangesContainer);
