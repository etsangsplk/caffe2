package predict

import (
	"bufio"
	"image"
	"os"
	"path/filepath"
	"strings"

	context "golang.org/x/net/context"

	"github.com/anthonynsimon/bild/parallel"
	opentracing "github.com/opentracing/opentracing-go"
	"github.com/pkg/errors"
	"github.com/rai-project/caffe"
	"github.com/rai-project/caffe2"
	"github.com/rai-project/config"
	"github.com/rai-project/dlframework"
	"github.com/rai-project/dlframework/framework/agent"
	common "github.com/rai-project/dlframework/framework/predict"
	"github.com/rai-project/downloadmanager"
	gocaffe2 "github.com/rai-project/go-caffe2"
	raiimage "github.com/rai-project/image"
)

type ImagePredictor struct {
	common.ImagePredictor
	workDir   string
	features  []string
	predictor *gocaffe2.Predictor
	inputDims []int32
}

func New(model dlframework.ModelManifest) (common.Predictor, error) {
	modelInputs := model.GetInputs()
	if len(modelInputs) != 1 {
		return nil, errors.New("number of inputs not supported")
	}
	firstInputType := modelInputs[0].GetType()
	if strings.ToLower(firstInputType) != "image" {
		return nil, errors.New("input type not supported")
	}
	predictor := new(ImagePredictor)
	return predictor.Load(context.Background(), model)
}

func (p *ImagePredictor) Load(ctx context.Context, model dlframework.ModelManifest) (common.Predictor, error) {
	framework, err := model.ResolveFramework()
	if err != nil {
		return nil, err
	}

	workDir, err := model.WorkDir()
	if err != nil {
		return nil, err
	}

	ip := &ImagePredictor{
		ImagePredictor: common.ImagePredictor{
			Base: common.Base{
				Framework: framework,
				Model:     model,
			},
		},
		workDir: workDir,
	}

	return ip, nil
}

func (p *ImagePredictor) GetWeightsUrl() string {
	model := p.Model
	if model.GetModel().GetIsArchive() {
		return model.GetModel().GetBaseUrl()
	}
	baseURL := ""
	if model.GetModel().GetBaseUrl() != "" {
		baseURL = strings.TrimSuffix(model.GetModel().GetBaseUrl(), "/") + "/"
	}
	return baseURL + model.GetModel().GetWeightsPath()
}

func (p *ImagePredictor) GetGraphUrl() string {
	model := p.Model
	if model.GetModel().GetIsArchive() {
		return model.GetModel().GetBaseUrl()
	}
	baseURL := ""
	if model.GetModel().GetBaseUrl() != "" {
		baseURL = strings.TrimSuffix(model.GetModel().GetBaseUrl(), "/") + "/"
	}
	return baseURL + model.GetModel().GetGraphPath()
}

func (p *ImagePredictor) GetFeaturesUrl() string {
	model := p.Model
	params := model.GetOutput().GetParameters()
	pfeats, ok := params["features_url"]
	if !ok {
		return ""
	}
	return pfeats.Value
}

func (p *ImagePredictor) GetGraphPath() string {
	model := p.Model
	graphPath := filepath.Base(model.GetModel().GetGraphPath())
	return filepath.Join(p.workDir, graphPath)
}

func (p *ImagePredictor) GetWeightsPath() string {
	model := p.Model
	graphPath := filepath.Base(model.GetModel().GetWeightsPath())
	return filepath.Join(p.workDir, graphPath)
}

func (p *ImagePredictor) GetFeaturesPath() string {
	model := p.Model
	return filepath.Join(p.workDir, model.GetName()+".features")
}

func (p *ImagePredictor) GetMeanPath() string {
	model := p.Model
	return filepath.Join(p.workDir, model.GetName()+".mean")
}

func (p *ImagePredictor) readMeanFromURL(ctx context.Context, url string) ([]float32, error) {
	targetPath := filepath.Join(p.workDir, "mean.binaryproto")
	fileName, err := downloadmanager.DownloadFile(ctx, url, targetPath)
	if err != nil {
		return nil, err
	}
	blob, err := caffe.ReadBlob(fileName)
	if err != nil {
		return nil, err
	}

	return blob.Data, nil
}

func (p *ImagePredictor) Preprocess(ctx context.Context, input interface{}) (interface{}, error) {
	if span, newCtx := opentracing.StartSpanFromContext(ctx, "Preprocess"); span != nil {
		ctx = newCtx
		defer span.Finish()
	}

	inputImage, ok := input.(image.Image)
	if !ok {
		return nil, errors.New("expecting an image input")
	}

	imageDims, err := p.GetImageDimensions()
	if err != nil {
		return nil, err
	}

	img, err := raiimage.Resize(ctx, inputImage, int(imageDims[2]), int(imageDims[3]))
	if err != nil {
		return nil, errors.Wrap(err, "failed to resize input image")
	}

	b := img.Bounds()
	height := b.Max.Y - b.Min.Y // image height
	width := b.Max.X - b.Min.X  // image width

	meanImage, err := p.GetMeanImage(ctx, p.readMeanFromURL)
	if err != nil {
		return nil, errors.Wrap(err, "failed to get mean image")
	}
	mean := [3]float32{0, 0, 0}
	if len(meanImage) != 3 {
		for cc := 0; cc < 3; cc++ {
			accum := float32(0)
			offset := cc * width * height
			for ii := 0; ii < height; ii++ {
				for jj := 0; jj < width; jj++ {
					accum += meanImage[offset+ii*width+jj]
				}
			}
			mean[cc] = accum / float32(width*height)
		}
	} else {
		copy(mean[:], meanImage[0:2])
	}

	res := make([]float32, 3*height*width)
	parallel.Line(height, func(start, end int) {
		w := width
		h := height
		for y := start; y < end; y++ {
			for x := 0; x < width; x++ {
				r, g, b, _ := img.At(x+b.Min.X, y+b.Min.Y).RGBA()
				res[y*w+x] = float32(b>>8) - 128       //mean[0]
				res[w*h+y*w+x] = float32(g>>8) - 128   // mean[1]
				res[2*w*h+y*w+x] = float32(r>>8) - 128 //mean[2]
			}
		}
	})
	return res, nil
}

func (p *ImagePredictor) Download(ctx context.Context) error {
	if span, newCtx := opentracing.StartSpanFromContext(ctx, "DownloadingModel"); span != nil {
		ctx = newCtx
		defer span.Finish()
	}

	if _, err := downloadmanager.DownloadFile(ctx, p.GetGraphUrl(), p.GetGraphPath()); err != nil {
		return err
	}
	if _, err := downloadmanager.DownloadFile(ctx, p.GetWeightsUrl(), p.GetWeightsPath()); err != nil {
		return err
	}
	if _, err := downloadmanager.DownloadFile(ctx, p.GetFeaturesUrl(), p.GetFeaturesPath()); err != nil {
		return err
	}
	return nil
}

func (p *ImagePredictor) loadPredictor(ctx context.Context) error {
	if p.predictor != nil {
		return nil
	}

	if span, newCtx := opentracing.StartSpanFromContext(ctx, "LoadPredictor"); span != nil {
		ctx = newCtx
		defer span.Finish()
	}

	var features []string
	f, err := os.Open(p.GetFeaturesPath())
	if err != nil {
		return errors.Wrapf(err, "cannot read %s", p.GetFeaturesPath())
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		features = append(features, line)
	}

	p.features = features

	p.inputDims, err = p.GetImageDimensions()
	if err != nil {
		return err
	}

	pred, err := gocaffe2.New(p.GetGraphPath(), p.GetWeightsPath())
	if err != nil {
		return err
	}
	p.predictor = pred

	return nil
}

func (p *ImagePredictor) Predict(ctx context.Context, input interface{}) (*dlframework.PredictionFeatures, error) {
	if span, newCtx := opentracing.StartSpanFromContext(ctx, "Predict"); span != nil {
		ctx = newCtx
		defer span.Finish()
	}
	if err := p.loadPredictor(ctx); err != nil {
		return nil, err
	}

	imageData, ok := input.([]float32)
	if !ok {
		return nil, errors.New("expecting []float32 input in predict function")
	}

	predictions, err := p.predictor.Predict(imageData, int(p.inputDims[1]), int(p.inputDims[2]), int(p.inputDims[3]))
	if err != nil {
		return nil, err
	}

	rprobs := make([]*dlframework.PredictionFeature, len(predictions))
	for ii, pred := range predictions {
		rprobs[ii] = &dlframework.PredictionFeature{
			Index:       int64(pred.Index),
			Name:        p.features[pred.Index],
			Probability: pred.Probability,
		}
	}
	res := dlframework.PredictionFeatures(rprobs)

	return &res, nil
}

func (p *ImagePredictor) Close() error {
	if p.predictor != nil {
		p.predictor.Close()
	}
	return nil
}

func init() {
	config.AfterInit(func() {
		framework := caffe2.FrameworkManifest
		agent.AddPredictor(framework, &ImagePredictor{
			ImagePredictor: common.ImagePredictor{
				Base: common.Base{
					Framework: framework,
				},
			},
		})
	})
}
