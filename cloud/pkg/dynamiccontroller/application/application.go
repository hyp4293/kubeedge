package application

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	beehiveContext "github.com/kubeedge/beehive/pkg/core/context"
	"github.com/kubeedge/beehive/pkg/core/model"
	"github.com/kubeedge/kubeedge/cloud/pkg/common/client"
	"github.com/kubeedge/kubeedge/cloud/pkg/common/modules"
	"github.com/kubeedge/kubeedge/cloud/pkg/dynamiccontroller/messagelayer"
	"github.com/kubeedge/kubeedge/edge/pkg/common/message"
	"github.com/kubeedge/kubeedge/edge/pkg/edgehub"
	"github.com/kubeedge/kubeedge/pkg/metaserver"
	metainternalversion "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	"k8s.io/klog/v2"
	"strings"
	"sync"
	"time"
)

// used to set Message.Route
const (
	MetaServerSource    = "metaserver"
	ApplicationResource = "application"
	ApplicationResp     = "applicationResponse"
	Ignore              = "ignore"
)

type applicationStatus string

const (
	// set by agent
	PreApplying applicationStatus = "PreApplying" // application is waiting to be sent to cloud
	InApplying  applicationStatus = "InApplying"  // application is sending to cloud

	// set by center
	InProcessing applicationStatus = "InProcessing" // application is in processing by cloud
	Approved     applicationStatus = "Approved"     // application is approved by cloud
	Rejected     applicationStatus = "Rejected"     // application is rejected by cloud

	// both
	Failed    applicationStatus = "Failed"    // failed to get application resp from cloud
	Completed applicationStatus = "Completed" // application is completed and waiting to be recycled
)

type applicationVerb string

const (
	Get    applicationVerb = "get"
	List   applicationVerb = "list"
	Watch  applicationVerb = "watch"
	Create applicationVerb = "create"
	Delete applicationVerb = "delete"
	Update applicationVerb = "update"
)

// record the resources that are in applying for requesting to be transferred down from the cloud, please:
// 0.use ApplicationAgent.Generate to generate application
// 1.use ApplicationAgent.Apply to apply application( generate msg and send it to cloud dynamiccontroller)
type application struct {
	Key      string // group version resource namespaces name
	Verb     applicationVerb
	Nodename string
	Status   applicationStatus
	Reason   string // why in this status
	Option   []byte //
	ReqBody  []byte // better a k8s api instance
	RespBody []byte

	selector LabelFieldSelector // if verb == list, option
	ctx      context.Context    // to end app.Wait
	cancel   context.CancelFunc
	//TODO: add lock
}

func newApplication(key string, verb applicationVerb, nodename string, option interface{}, selector LabelFieldSelector, reqBody interface{}) *application {
	var v1 metav1.ListOptions
	if internal, ok := option.(metainternalversion.ListOptions); ok {
		err := metainternalversion.Convert_internalversion_ListOptions_To_v1_ListOptions(&internal, &v1, nil)
		if err != nil {
			// error here won't happen, log in case
			klog.Errorf("failed to transfer internalListOption to v1ListOption, force set to empty")
		}
	}
	app := &application{
		Key:      key,
		Verb:     verb,
		Nodename: nodename,
		selector: selector,
		Status:   PreApplying,
		Option:   toBytes(v1),
		ReqBody:  toBytes(reqBody),
	}
	return app
}

func (a *application) String() string {
	split := ";"
	return strings.Join([]string{a.Nodename, a.Key, string(a.Verb), a.selector.String()}, split)
}
func (a *application) ReqContent() interface{} {
	return a.ReqBody
}
func (a *application) RespContent() interface{} {
	return a.RespBody
}

func (a *application) ToListener(option metav1.ListOptions) *SelectorListener {
	gvr, namespace, _ := metaserver.ParseKey(a.Key)
	selector := NewSelector(option.LabelSelector, option.FieldSelector)
	if namespace != "" {
		selector.Field = fields.AndSelectors(selector.Field, fields.OneTermEqualSelector("metadata.namespace", namespace))
	}
	l := NewSelectorListener(a.Nodename, gvr, selector)
	return l
}

// remember i must be a pointer to the initialized variable
func (a *application) OptionTo(i interface{}) error {
	err := json.Unmarshal(a.Option, i)
	if err != nil {
		return fmt.Errorf("failed to prase Option bytes, %v", err)
	}
	return nil
}

func (a *application) ReqBodyTo(i interface{}) error {
	err := json.Unmarshal(a.ReqBody, i)
	if err != nil {
		return fmt.Errorf("failed to parse ReqBody bytes, %v", err)
	}
	return nil
}

func (a *application) RespBodyTo(i interface{}) error {
	err := json.Unmarshal(a.RespBody, i)
	if err != nil {
		return fmt.Errorf("failed to parse RespBody bytes, %v", err)
	}
	return nil
}

//
func (a *application) GVR() schema.GroupVersionResource {
	gvr, _, _ := metaserver.ParseKey(a.Key)
	return gvr
}
func (a *application) Namespace() string {
	_, ns, _ := metaserver.ParseKey(a.Key)
	return ns
}

func (a *application) Labels() labels.Selector {
	return a.selector.Labels()
}

func (a *application) Fields() fields.Selector {
	return a.selector.Fields()
}

func (a *application) Call() {
	if a.cancel != nil {
		a.cancel()
	}
	return
}

func (a *application) getStatus() applicationStatus {
	return a.Status
}

// Wait the result of application after it is applied by application agent
func (a *application) Wait() applicationStatus {
	if a.ctx != nil {
		<-a.ctx.Done()
	}
	return a.getStatus()
}

// Close must be called by who generated this application or GC()
func (a *application) Close() {
	a.Status = Completed
}

// used for generating application and do apply
type ApplicationAgent struct {
	Applications sync.Map //store struct application
	nodeName     string
}

// edged config.Config.HostnameOverride
func NewApplicationAgent(nodeName string) *ApplicationAgent {
	return &ApplicationAgent{nodeName: nodeName}
}

func (a *ApplicationAgent) Generate(ctx context.Context, verb applicationVerb, option interface{}, lf LabelFieldSelector, obj runtime.Object) *application {
	key, err := metaserver.KeyFuncReq(ctx, "")
	if err != nil {
		klog.Errorf("%v", err)
		return &application{}
	}
	app := newApplication(key, verb, a.nodeName, option, lf, obj)
	store, ok := a.Applications.LoadOrStore(app.String(), app)
	if ok {
		return store.(*application)
	}
	return app
}

func (a *ApplicationAgent) Apply(app *application) error {
	store, ok := a.Applications.Load(app.String())
	if !ok {
		return fmt.Errorf("application %v has not been registered to agent", app.String())
	}
	app = store.(*application)
	switch app.getStatus() {
	case PreApplying, Completed:
		ctx, cancle := context.WithCancel(beehiveContext.GetContext())
		go a.doApply(app, ctx, cancle)
	case Rejected, Failed:
		return errors.New(app.Reason)
	case Approved:
		return nil
	case InApplying:
		//continue
	}
	if status := app.Wait(); status != Approved {
		return errors.New(app.Reason)
	}
	return nil
}

func (a *ApplicationAgent) doApply(app *application, ctx context.Context, cancelFunc context.CancelFunc) {
	// for reusing application, make sure last context done, or return
	if app.ctx != nil && app.ctx.Err() == nil {
		return
	}
	// clean and reset
	app.Reason = ""
	app.RespBody = []byte{}
	app.ctx = ctx
	app.cancel = cancelFunc
	defer app.Call()

	// encapsulate as a message
	msg := model.NewMessage("").SetRoute(MetaServerSource, modules.DynamicControllerModuleGroup).FillBody(*app)
	msg.SetResourceOperation("null", "null")
	app.Status = InApplying
	resp, err := beehiveContext.SendSync(edgehub.ModuleNameEdgeHub, *msg, 10*time.Second)
	if err != nil {
		app.Status = Failed
		app.Reason = fmt.Sprintf("failed to access cloud application center: %v", err)
		return
	}

	retApp, err := msgToApplication(resp)
	if err != nil {
		app.Status = Failed
		app.Reason = fmt.Sprintf("failed to get application from resp msg: %v", err)
		return
	}

	//merge returned application to local application
	app.Status = retApp.Status
	app.Reason = retApp.Reason
	app.RespBody = retApp.RespBody
}

func (a *ApplicationAgent) GC() {

}

type ApplicationCenter struct {
	Applications sync.Map
	HandlerCenter
	messageLayer messagelayer.MessageLayer
	kubeclient   dynamic.Interface
}

func NewApplicationCenter(dynamicSharedInformerFactory dynamicinformer.DynamicSharedInformerFactory) *ApplicationCenter {
	a := &ApplicationCenter{
		HandlerCenter: NewHandlerCenter(dynamicSharedInformerFactory),
		kubeclient:    client.GetDynamicClient(),
		messageLayer:  messagelayer.NewContextMessageLayer(),
	}
	return a

}

func toBytes(i interface{}) []byte {
	if i == nil {
		return []byte{}
	}
	var bytes []byte
	var err error
	switch i.(type) {
	case []byte:
		bytes = i.([]byte)
	default:
		bytes, err = json.Marshal(i)
		if err != nil {
			klog.Fatalf("marshal content to []byte failed, err: %v", err)
		}
	}
	return bytes
}

// extract application in message's Content
func msgToApplication(msg model.Message) (*application, error) {
	var app = new(application)
	var err error
	err = json.Unmarshal(toBytes(msg.Content), app)
	if err != nil {
		//klog.Errorf("%v", err)
		return nil, err
	}
	return app, nil
}

// TODO: upgrade to parallel process
// Process translate msg to application , process and send resp to edge
func (c *ApplicationCenter) Process(msg model.Message) {
	app, err := msgToApplication(msg)
	if err != nil {
		klog.Errorf("failed to translate msg to application: %v", err)
		return
	}
	klog.Infof("[metaserver/ApplicationCenter] get a application %v", app.String())
	gvr, ns, name := metaserver.ParseKey(app.Key)
	err = func() error {
		app.Status = InProcessing
		switch app.Verb {
		case List:
			var option = new(metav1.ListOptions)
			if err := app.OptionTo(option); err != nil {
				return err
			}
			err := c.HandlerCenter.AddListener(app.ToListener(*option))
			if err != nil {
				return fmt.Errorf("failed to add listener, %v", err)
			}
			list, err := c.kubeclient.Resource(app.GVR()).Namespace(app.Namespace()).List(context.TODO(), *option)
			if err != nil {
				return fmt.Errorf("successfuly to add listener but failed to get current list, %v", err)
			}
			c.Response(app, msg.GetID(), Approved, "", list)
		case Watch:
			var option = new(metav1.ListOptions)
			if err := app.OptionTo(option); err != nil {
				return err
			}
			err := c.HandlerCenter.AddListener(app.ToListener(*option))
			if err != nil {
				return fmt.Errorf("failed to add listener, %v", err)
			}
			c.Response(app, msg.GetID(), Approved, "", nil)
		case Get:
			var option = new(metav1.GetOptions)
			if err := app.OptionTo(option); err != nil {
				return err
			}
			retObj, err := c.kubeclient.Resource(gvr).Namespace(ns).Get(context.TODO(), name, *option)
			if err != nil {
				return err
			}
			c.Response(app, msg.GetID(), Approved, "", retObj)
		case Create:
			var option = new(metav1.CreateOptions)
			if err := app.OptionTo(option); err != nil {
				return err
			}
			var obj = new(unstructured.Unstructured)
			if err := app.ReqBodyTo(obj); err != nil {
				return err
			}
			retObj, err := c.kubeclient.Resource(gvr).Namespace(ns).Create(context.TODO(), obj, *option)
			if err != nil {
				return err
			}
			c.Response(app, msg.GetID(), Approved, "", retObj)
		case Delete:
			var option = new(metav1.DeleteOptions)
			if err := app.OptionTo(&option); err != nil {
				return err
			}
			var obj = new(unstructured.Unstructured)
			if err := app.ReqBodyTo(obj); err != nil {
				return err
			}
			err := c.kubeclient.Resource(gvr).Namespace(ns).Delete(context.TODO(), name, *option)
			if err != nil {
				return err
			}
			c.Response(app, msg.GetID(), Approved, "", nil)
		case Update:
			var option = new(metav1.UpdateOptions)
			if err := app.OptionTo(option); err != nil {
				return err
			}
			var obj = new(unstructured.Unstructured)
			if err := app.ReqBodyTo(obj); err != nil {
				return err
			}
			retObj, err := c.kubeclient.Resource(gvr).Namespace(ns).Update(context.TODO(), obj, *option)
			if err != nil {
				return err
			}
			c.Response(app, msg.GetID(), Approved, "", retObj)
		default:
			return fmt.Errorf("unsupported application Verb type :%v", app.Verb)
		}
		return nil
	}()
	if err != nil {
		c.Response(app, msg.GetID(), Rejected, app.Reason, nil)
		klog.Errorf("[metaserver/applicationCenter]failed to process application(%v), %v", *app, err)
	}
}

// Response update application, generate and send resp message to edge
func (c *ApplicationCenter) Response(app *application, parentID string, status applicationStatus, reason string, respContent interface{}) {
	app.Status = status
	app.Reason = reason
	if respContent != nil {
		app.RespBody = toBytes(respContent)
	}

	msg := model.NewMessage(parentID)
	msg.Content = *app
	resource, err := messagelayer.BuildResource(app.Nodename, Ignore, ApplicationResource, Ignore)
	if err != nil {
		klog.Warningf("built message resource failed with error: %s", err)
		return
	}
	msg.BuildRouter(modules.DynamicControllerModuleName, message.ResourceGroupName, resource, ApplicationResp)
	if err := c.messageLayer.Response(*msg); err != nil {
		klog.Warningf("send message failed with error: %s, operation: %s, resource: %s", err, msg.GetOperation(), msg.GetResource())
	} else {
		klog.V(4).Infof("send message successfully, operation: %s, resource: %s", msg.GetOperation(), msg.GetResource())
	}

}

func (c *ApplicationCenter) GC() {

}
